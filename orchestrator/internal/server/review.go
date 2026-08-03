package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/textutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
)

// jobRoleDeveloper, jobRoleReviewer and jobRolePlanner are the three values
// app.jobs.role carries (028_workflow_run_ai_review.sql's CHECK constraint,
// widened by 029_workflow_run_planning_status.sql to add 'planner' for #351).
// Every job that ever existed before the role column was added is a developer
// job by definition -- that is the column's DB default -- ScheduleOnce is the
// only writer of jobRolePlanner (for a project that opted into RequirePlanning),
// and dispatchReviewerJob is the only writer of jobRoleReviewer.
const (
	jobRoleDeveloper = "developer"
	jobRoleReviewer  = "reviewer"
	jobRolePlanner   = "planner"
)

// reviewExecutionID derives the execution ID a reopened job's reviewer
// execution reports back against. Distinct per review cycle (rather than a
// single fixed suffix the way implementExecutionID's "-implement" is) so a
// repeat review -- reachable now that the repair loop (#354, repair.go's
// dispatchRepairJob) dispatches a second developer attempt and a second
// review after it -- never reuses an execution ID already retired by an
// earlier RecordJobExecutionEvent call.
func reviewExecutionID(jobID string, cycle int32) string {
	return fmt.Sprintf("%s-review-%d", jobID, cycle)
}

// reviewerPacket builds the one execution dispatchReviewerJob offers: role
// "reviewer", every constraint the runner's taskpacket.Validate enforces for
// that role false (it may not modify files, push or merge -- see
// runner/internal/taskpacket/taskpacket.go), checking out the same branch the
// developer packet published rather than the developer's own commit, since a
// V1 review is deliberately given a fresh checkout and no access to the
// developer's session (AGENTS.md: "use fresh context for independent AI
// review") instead of a diff computed here. acceptanceCriteria and
// reviewFindings are left empty for the same reason developerPacket's are: V1
// has no planning phase to populate the former, and a reviewer packet itself
// has no prior findings of its own to report -- repair.go's repairPacket is
// what populates that field, for the developer-role repair attempt a
// rejection dispatches, not for the reviewer packet reviewing it.
func reviewerPacket(jobID string, cycle int32, projectID, externalID, title, body, mode, repositoryURL, localPath, defaultBranch, branch, executionImage string, timeoutSeconds int) (map[string]any, error) {
	if !idgen.ValidID(jobID) || !idgen.ValidID(projectID) || externalID == "" || title == "" || defaultBranch == "" || branch == "" || (mode == "managed_clone" && repositoryURL == "") || (mode == "existing_path" && localPath == "") || (mode != "managed_clone" && mode != "existing_path") || timeoutSeconds < 1 {
		return nil, status.Error(codes.FailedPrecondition, "review dispatch is invalid")
	}
	return map[string]any{
		"protocolVersion": "1.0", "jobId": jobID, "executionId": reviewExecutionID(jobID, cycle), "role": "reviewer",
		"objective":  "Independently review the implementation of " + externalID + ": " + title,
		"issue":      map[string]string{"externalId": externalID, "title": title, "body": body},
		"repository": map[string]string{"projectId": projectID, "mode": mode, "url": repositoryURL, "localPath": localPath, "defaultBranch": defaultBranch, "branch": branch},
		"promptPath": ".loop/prompt.md", "expectedOutput": ".loop/result.json", "timeoutSeconds": timeoutSeconds, "environmentRefs": []any{}, "executionImage": executionImage,
		"constraints": map[string]bool{"mayModifyFiles": false, "mayPush": false, "mayMerge": false}, "pipeline": []any{}, "acceptanceCriteria": []string{}, "plan": []string{}, "previousFailures": []string{}, "currentCommit": "", "diffSummary": "", "failedChecks": []string{}, "reviewFindings": []string{},
	}, nil
}

// aiReviewEnabled reads the project's EnableAiReview configuration (server.go's
// projectConfig) for the project a workflow run belongs to. Called from inside
// persistExecutionEvent's own transaction so the decision and the status
// transition it drives land atomically together; queries is that
// transaction-scoped *db.Queries; passing s.queries directly (untransacted)
// is equally correct for any other caller.
func aiReviewEnabled(ctx context.Context, queries *db.Queries, workflowID string) (bool, error) {
	configuration, err := queries.GetProjectConfigForWorkflow(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(configuration, &config); err != nil {
		return false, configurationError(err)
	}
	return config.EnableAiReview, nil
}

// dispatchReviewerJob reopens a workflow run's one job for a second,
// independent execution -- a fresh reviewer session, per AGENTS.md's "use
// fresh context for independent AI review" -- once its developer execution
// has completed and the project opted into EnableAiReview. Called inline by
// persistExecutionEvent right after its own transaction commits (the same
// shape deliverWorkflow is already called in), and again by
// resumeStrandedReviewDispatches (recovery.go) for a run that call failed to
// dispatch, typically for the mundane reason that no runner was connected
// yet.
//
// Every step is a guarded, idempotent no-op if it finds nothing to do:
// GetReviewDispatchWorkflow's WHERE clause (review.sql) only ever matches a
// run still sitting at 'waiting_ai_review' with its job still the completed
// developer job, so a second concurrent call -- the recovery sweep racing
// this one -- finds no row, or loses ReopenJobForReview's own guard, and
// simply returns nil.
func (s *Core) dispatchReviewerJob(ctx context.Context, workflowID string) error {
	runners := s.connectedRunners()
	if len(runners) == 0 {
		return nil // resumeStrandedReviewDispatches retries once a runner connects
	}
	row, err := s.queries.GetReviewDispatchWorkflow(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(row.Configuration, &config); err != nil {
		return configurationError(err)
	}
	// config.Labels decodes to nil, not an empty slice, when the project's
	// configuration never set required_runner_labels -- json.Marshal(nil)
	// renders that as the JSON literal `null`, and `labels @> 'null'::jsonb`
	// is never true, which would silently make every runner ineligible.
	labels := config.Labels
	if labels == nil {
		labels = []string{}
	}
	requiredLabels, err := jsonLabels(labels)
	if err != nil {
		return err
	}
	runnerID, err := s.queries.SelectEligibleReviewRunner(ctx, db.SelectEligibleReviewRunnerParams{
		RunnerIds: runners, RequiredLabels: []byte(requiredLabels),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // resumeStrandedReviewDispatches retries once an eligible runner is free
	}
	if err != nil {
		return databaseError(err)
	}
	cycle := row.ReviewCycles + 1
	packet, err := reviewerPacket(row.JobID, cycle, row.ProjectID, row.ExternalID, row.Title, row.Body, row.RepositoryMode, row.RepositoryUrl, row.LocalRepositoryPath, row.DefaultBranch, row.BranchName, config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds))
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(packet)
	if err != nil {
		return databaseError(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	leaseGeneration, err := queries.ReopenJobForReview(ctx, db.ReopenJobForReviewParams{RunnerID: runnerID, ID: row.JobID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // raced with another dispatch attempt for this same job
	}
	if err != nil {
		return databaseError(err)
	}
	if err := queries.IncrementReviewCycles(ctx, workflowID); err != nil {
		return databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: idgen.NewID(), JobID: row.JobID, RunnerID: runnerID}); err != nil {
		return databaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: &runnerv1.JobOffer{JobId: row.JobID, LeaseGeneration: leaseGeneration, TaskPacketJson: string(encoded)}}}
	s.enqueue(runnerID, message)
	// A failed enqueue (the runner disconnected between SelectEligibleReviewRunner
	// and here) leaves the job at 'offered' with nothing to answer it, exactly
	// the shape ReclaimUnansweredReviewOffers already reclaims on its own
	// timeout -- unlike ScheduleOnce's releaseUndeliveredOffer, nothing needs
	// undoing inline: the developer's work is already done and must not be
	// discarded just because this particular offer needs a retry.
	return nil
}

// reviewVerdict reads an independent reviewer's own completed-execution
// payload for the verdict and findings its result.json wrote, per
// dispatch.go's reviewerPromptFor/expectedOutputSchema
// ("protocolVersion, executionId, status, summary, verdict, acceptanceCriteria,
// and findings"). The runner nests a role's extra fields under payload.result
// (runner/internal/dispatch/control_loop.go's terminalPayload sets
// payload["result"] = result.Raw) -- the same "raw" passthrough
// agentBlockReason reads "blocked" from at the top level for a developer's
// payload, just one level deeper here. Best effort, like agentBlockReason: a
// missing or wrongly typed field simply comes back empty rather than failing
// the event.
func reviewVerdict(payloadJSON string) (verdict string, findings []string) {
	var payload struct {
		Result struct {
			Verdict  string   `json:"verdict"`
			Findings []string `json:"findings"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(payloadJSON), &payload)
	return strings.ToLower(strings.TrimSpace(payload.Result.Verdict)), payload.Result.Findings
}

// reviewApproved classifies a reviewVerdict result as an approval. Anything
// else -- an explicit rejection, an empty or unrecognised verdict -- is not:
// an independent review that did not clearly approve must not be treated as
// silent success.
func reviewApproved(verdict string) bool {
	switch verdict {
	case "approved", "approve", "pass", "passed", "accept", "accepted":
		return true
	}
	return false
}

// extractFinalRevision reads the commit a completed execution's own payload
// reported (runner/internal/dispatch/control_loop.go's terminalPayload sets
// the top-level "finalRevision" field for every role). A reviewer packet
// never commits, so this is meaningful only for a developer's own completed
// event -- dispatchReviewerJob has no diff or commit of its own to report, so
// V1's reviewer packet leaves currentCommit/diffSummary empty and relies on
// the prompt's own fallback ("Inspect the repository diff independently",
// dispatch.go's reviewerPromptFor) instead of depending on this value.
func extractFinalRevision(payloadJSON string) string {
	var payload struct {
		FinalRevision string `json:"finalRevision"`
	}
	_ = json.Unmarshal([]byte(payloadJSON), &payload)
	return payload.FinalRevision
}

// reviewResultJSON re-encodes a reviewer's payload.result object for
// app.ai_reviews.result (jsonb), falling back to an empty object for a
// payload that carried no result at all -- an agent-declared crash, or one
// that wrote nothing recognisable to .loop/result.json. Never fails the
// caller: like every other payload reader here, the payload is agent-supplied
// and must not be able to strand a review's own bookkeeping.
func reviewResultJSON(payloadJSON string) []byte {
	var payload struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil || len(payload.Result) == 0 {
		return []byte("{}")
	}
	if !json.Valid(payload.Result) {
		return []byte("{}")
	}
	return payload.Result
}

// reviewRejectionReason composes the blocking_reason a rejected review ends a
// run at (once repairOrBlock decides not to repair it, or exhausts its
// bound), in the same bounded, one-line shape agentBlockReason's
// composeBlockReason already gives a developer's own declared block.
func reviewRejectionReason(findings []string) string {
	return textutil.Truncate(composeReviewReason(findings), maxBlockingReasonBytes)
}

func composeReviewReason(findings []string) string {
	const prefix = "independent AI review did not approve the change"
	if len(findings) == 0 {
		return prefix
	}
	return prefix + " (findings: " + strings.Join(findings, "; ") + ")"
}

// handleReviewCompletion runs once persistExecutionEvent's own transaction has
// committed a reviewer's "completed" event -- the same after-commit shape
// deliverWorkflow already runs in for a developer's "completed" event. It
// records the verdict in app.ai_reviews (reserved, unused until #353) and
// either lets the run proceed to delivery (an approving verdict, the same
// deliverWorkflow call a project with AI review disabled always used) or
// hands the rejection to repairOrBlock (repair.go, #354): a project that
// opted into the repair loop and still has attempts left gets a bounded,
// findings-informed repair dispatch instead; every other rejection still ends
// the run at StatusBlocked with the reviewer's findings as the reason, the
// same terminal shape a failed deterministic pipeline check or an agent's own
// declared block already use.
func (s *Core) handleReviewCompletion(ctx context.Context, workflowID, payloadJSON string) error {
	verdict, findings := reviewVerdict(payloadJSON)
	recordedVerdict := verdict
	if recordedVerdict == "" {
		recordedVerdict = "unknown"
	}
	if err := s.queries.InsertAiReview(ctx, db.InsertAiReviewParams{
		ID: idgen.NewID(), WorkflowRunID: workflowID, CommitSha: extractFinalRevision(payloadJSON),
		Verdict: recordedVerdict, Result: reviewResultJSON(payloadJSON),
	}); err != nil {
		return databaseError(err)
	}
	if !reviewApproved(verdict) {
		return s.repairOrBlock(ctx, workflowID, reviewRejectionReason(findings))
	}
	return s.approveReviewAndDeliver(ctx, workflowID)
}

// approveReviewAndDeliver hands an approved review off to deliverWorkflow,
// which requires the run to be sitting at StatusDelivering (MarkWorkflowDelivered's
// own guard) -- the same status a developer's own "completed" event moves a
// run through when AI review is disabled. MarkWorkflowReviewApproved's guard
// makes the transition a no-op, not a double delivery, if this ever raced
// another caller (this same decision re-applied by
// resumeStrandedReviewVerdicts, for one).
func (s *Core) approveReviewAndDeliver(ctx context.Context, workflowID string) error {
	affected, err := s.queries.MarkWorkflowReviewApproved(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if affected != 1 {
		return nil
	}
	return s.deliverWorkflow(ctx, workflowID)
}

// applyRecordedReviewVerdict is resumeStrandedReviewVerdicts' (recovery.go)
// per-workflow action: a reviewer execution already finished and
// persistExecutionEvent already recorded its terminal event, but the process
// died before handleReviewCompletion's own follow-on landed. Re-derives the
// same decision from the verdict app.ai_reviews already has on file instead of
// re-parsing the original payload (which is not re-fetched here), so this
// never inserts a second app.ai_reviews row for the same completed execution.
// The rare case of no recorded verdict at all (a crash before
// handleReviewCompletion's own InsertAiReview call landed) blocks rather than
// waiting forever: there is nothing left to re-derive a verdict from.
func (s *Core) applyRecordedReviewVerdict(ctx context.Context, workflowID string) error {
	verdict, err := s.queries.GetLatestAiReview(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "ai_review.rejected", "independent review completed but recorded no verdict")
	}
	if err != nil {
		return databaseError(err)
	}
	if reviewApproved(verdict) {
		return s.approveReviewAndDeliver(ctx, workflowID)
	}
	return s.repairOrBlock(ctx, workflowID, "independent AI review did not approve the change")
}
