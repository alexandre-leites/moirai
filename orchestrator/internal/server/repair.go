package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/textutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
)

// pipelineFailureReason reads whether a developer (or repair) execution's own
// terminal payload carries a pipeline failure -- runner/internal/dispatch/
// control_loop.go's terminalPayload sets payload["pipelineResults"] to the
// first failing (or timed-out) required command's own account, and only ever
// for a run whose agent itself completed and whose pipeline then failed (see
// dispatch.go: the pipeline only runs "if executeErr == nil && result.Status
// == completed", so this can never fire for an agent's own declared block or
// crash). Best effort, like agentBlockReason: a missing or wrongly typed
// field simply comes back "found nothing" rather than failing the event.
func pipelineFailureReason(payloadJSON string) (string, bool) {
	var payload struct {
		PipelineResults []struct {
			Command  string `json:"command"`
			ExitCode int    `json:"exitCode"`
			TimedOut bool   `json:"timedOut"`
		} `json:"pipelineResults"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil || len(payload.PipelineResults) == 0 {
		return "", false
	}
	failure := payload.PipelineResults[0]
	reason := fmt.Sprintf("required pipeline command failed with exit code %d: %s", failure.ExitCode, failure.Command)
	if failure.TimedOut {
		reason = "required pipeline command timed out: " + failure.Command
	}
	return textutil.Truncate(reason, maxBlockingReasonBytes), true
}

// maxRepairAttempts bounds how many bounded, AI-review-informed repair
// attempts repairOrBlock will dispatch for a single workflow run before
// giving up and falling through to StatusBlocked. Unlike
// maxDeliveryAttempts (delivery.go), which absorbs cheap, mechanical network
// retries, each repair attempt is a full agent execution: real wall-clock
// time and runner capacity spent on an issue the agent may simply not be
// equipped to fix. Two attempts -- each seeded with the previous reviewer's
// concrete findings, not a blind re-run -- catches the common case (a
// reviewer flagged something genuinely fixable) while still failing fast
// toward human attention when the same issue survives two informed tries in
// a row, rather than burning the fleet on a run that was never going to
// converge.
const maxRepairAttempts int32 = 2

// repairEligible decides whether a rejected review should be repaired instead
// of blocked: the project must have opted into EnableRepairLoop
// (projectConfig) and this run must not have already spent its bound of
// repair attempts. Called by repairOrBlock before dispatchRepairJob's own
// guarded query runs, so "not eligible" (the common case: repair loops
// default to off, the same opt-in shape EnableAiReview and
// RequireHumanApproval already use) reliably blocks immediately instead of
// silently leaving the run at 'waiting_ai_review' forever because
// dispatchRepairJob's own query found nothing to match.
func repairEligible(ctx context.Context, queries *db.Queries, workflowID string) (bool, error) {
	row, err := queries.GetWorkflowRepairState(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(row.Configuration, &config); err != nil {
		return false, configurationError(err)
	}
	return config.EnableRepairLoop && row.CiRepairAttempts < maxRepairAttempts, nil
}

// repairOrBlock is handleReviewCompletion's and applyRecordedReviewVerdict's
// shared decision for a rejected (or unresolvable) independent review: repair
// it, bounded and informed by the reviewer's own findings, if the project
// opted in and the run has attempts left; otherwise end the run at
// StatusBlocked exactly as V1 always has, with blockReason as the recorded
// cause.
func (s *Core) repairOrBlock(ctx context.Context, workflowID, blockReason string) error {
	eligible, err := repairEligible(ctx, s.queries, workflowID)
	if err != nil {
		return err
	}
	if eligible {
		return s.dispatchRepairJob(ctx, workflowID)
	}
	return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "ai_review.rejected", blockReason)
}

// repairExecutionID derives the execution ID a reopened job's repair
// execution reports back against, distinct per attempt (like
// reviewExecutionID's per-cycle suffix) so a second repair attempt never
// reuses an execution ID a previous attempt's RecordJobExecutionEvent call
// already retired.
func repairExecutionID(jobID string, attempt int32) string {
	return fmt.Sprintf("%s-repair-%d", jobID, attempt)
}

// repairPacket builds a repair attempt's execution: the same developer-role
// shape developerPacket dispatches for a project's first attempt
// (mayModifyFiles/mayPush true, mayMerge false -- delivery is always the
// orchestrator's own decision, never the agent's) but with the packet's
// existing reviewFindings field actually populated -- from the independent
// reviewer's own verdict on the attempt this one is repairing, or from the
// deterministic pipeline's own failing command (#352, dispatchPipelineRepairJob)
// -- instead of the empty slice developerPacket and reviewerPacket both leave
// it at today. executionID and objective are supplied by the caller rather
// than derived here, since the two triggers need distinct execution ID
// namespaces (repairExecutionID vs pipelineRepairExecutionID) so a run that
// spends attempts from both ci_repair_attempts and pipeline_repair_attempts
// over its life never reuses an execution ID either one already retired.
// pipeline is threaded through like developerPacket's own, so a repair
// attempt is gated by the project's deterministic pipeline exactly as the
// original attempt was.
func repairPacket(jobID, executionID string, attempt int32, projectID, externalID, title, body, mode, repositoryURL, localPath, defaultBranch, branch, executionImage string, timeoutSeconds int, objective string, pipeline []map[string]any, findings []string) (map[string]any, error) {
	if !idgen.ValidID(jobID) || !idgen.ValidID(projectID) || externalID == "" || title == "" || defaultBranch == "" || branch == "" || (mode == "managed_clone" && repositoryURL == "") || (mode == "existing_path" && localPath == "") || (mode != "managed_clone" && mode != "existing_path") || timeoutSeconds < 1 {
		return nil, status.Error(codes.FailedPrecondition, "repair dispatch is invalid")
	}
	if findings == nil {
		findings = []string{}
	}
	if pipeline == nil {
		pipeline = []map[string]any{}
	}
	return map[string]any{
		"protocolVersion": "1.0", "jobId": jobID, "executionId": executionID, "role": "developer",
		"objective":  fmt.Sprintf("Repair attempt %d: %s for %s: %s", attempt, objective, externalID, title),
		"issue":      map[string]string{"externalId": externalID, "title": title, "body": body},
		"repository": map[string]string{"projectId": projectID, "mode": mode, "url": repositoryURL, "localPath": localPath, "defaultBranch": defaultBranch, "branch": branch},
		"promptPath": ".loop/prompt.md", "expectedOutput": ".loop/result.json", "timeoutSeconds": timeoutSeconds, "environmentRefs": repositoryEnvironmentRefs(mode, repositoryURL), "executionImage": executionImage,
		"constraints": map[string]bool{"mayModifyFiles": true, "mayPush": true, "mayMerge": false}, "pipeline": pipeline, "acceptanceCriteria": []string{}, "plan": []string{}, "previousFailures": []string{}, "currentCommit": "", "diffSummary": "", "failedChecks": []string{}, "reviewFindings": findings,
	}, nil
}

// reviewRepairObjective and pipelineRepairObjective are the trailing clause
// repairPacket's objective sentence uses for each trigger -- see
// dispatchRepairJob and dispatchPipelineRepairJob respectively.
const (
	reviewRepairObjective   = "address independent AI review findings"
	pipelineRepairObjective = "address the deterministic pipeline failure"
)

// findingsFromReviewResult reads the findings array out of an app.ai_reviews
// row's own result column (reviewResultJSON's stored shape: the reviewer's
// payload.result, verbatim). Best effort, like reviewVerdict: a missing or
// wrongly typed field simply comes back empty rather than failing dispatch.
func findingsFromReviewResult(resultJSON []byte) []string {
	var result struct {
		Findings []string `json:"findings"`
	}
	_ = json.Unmarshal(resultJSON, &result)
	return result.Findings
}

// dispatchRepairJob reopens a workflow run's one job for a third role churn
// -- back to 'developer', for a repair attempt -- once repairOrBlock has
// decided the run is eligible. Called inline by repairOrBlock right after a
// review rejection, and again by resumeStrandedReviewVerdicts
// (recovery.go, via applyRecordedReviewVerdict) for a run whose inline call
// found no connected runner yet -- the same "waiting_ai_review, reviewer job
// completed" shape that sweep already re-derives a verdict for, so no
// separate stranded-repair sweep is needed: a repair dispatch that could not
// find a runner leaves the run in exactly the row shape that sweep already
// revisits.
//
// Every step is a guarded, idempotent no-op if it finds nothing to do, the
// same shape dispatchReviewerJob (review.go) already established.
func (s *Core) dispatchRepairJob(ctx context.Context, workflowID string) error {
	runners := s.connectedRunners()
	if len(runners) == 0 {
		return nil // resumeStrandedReviewVerdicts retries once a runner connects
	}
	row, err := s.queries.GetRepairDispatchWorkflow(ctx, db.GetRepairDispatchWorkflowParams{ID: workflowID, MaxRepairAttempts: maxRepairAttempts})
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
	// See dispatchReviewerJob's identical comment: config.Labels decodes to nil,
	// not an empty slice, when the project never set required_runner_labels.
	labels := config.Labels
	if labels == nil {
		labels = []string{}
	}
	requiredLabels, err := jsonLabels(labels)
	if err != nil {
		return err
	}
	// The eligible-runner check has nothing reviewer-specific about it (online,
	// enabled, not draining, not revoked, under capacity, labels match) -- it is
	// reused here rather than duplicated as a repair-scoped copy.
	runnerID, err := s.queries.SelectEligibleReviewRunner(ctx, db.SelectEligibleReviewRunnerParams{
		RunnerIds: runners, RequiredLabels: []byte(requiredLabels),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // resumeStrandedReviewVerdicts retries once an eligible runner is free
	}
	if err != nil {
		return databaseError(err)
	}
	attempt := row.CiRepairAttempts + 1
	findings := findingsFromReviewResult(row.LatestReviewResult)
	pipeline, err := pipelineStepsForPacket(ctx, s.queries, row.ProjectID)
	if err != nil {
		return err
	}
	packet, err := repairPacket(row.JobID, repairExecutionID(row.JobID, attempt), attempt, row.ProjectID, row.ExternalID, row.Title, row.Body, row.RepositoryMode, row.RepositoryUrl, row.LocalRepositoryPath, row.DefaultBranch, row.BranchName, config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds), reviewRepairObjective, pipeline, findings)
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
	affected, err := queries.MarkWorkflowRepairing(ctx, db.MarkWorkflowRepairingParams{ID: workflowID, MaxRepairAttempts: maxRepairAttempts})
	if err != nil {
		return databaseError(err)
	}
	if affected != 1 {
		return nil // raced away from 'waiting_ai_review', or the bound was already reached
	}
	leaseGeneration, err := queries.ReopenJobForRepair(ctx, db.ReopenJobForRepairParams{RunnerID: runnerID, ID: row.JobID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // raced with another dispatch attempt for this same job
	}
	if err != nil {
		return databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: idgen.NewID(), JobID: row.JobID, RunnerID: runnerID}); err != nil {
		return databaseError(err)
	}
	if err := queries.CreateWorkflowEvent(ctx, db.CreateWorkflowEventParams{
		WorkflowRunID: workflowID, EventType: "workflow_transition", Severity: "info",
		Column4: []byte(fmt.Sprintf(`{"reason":"dispatching repair attempt %d of %d, informed by the independent review's findings"}`, attempt, maxRepairAttempts)),
	}); err != nil {
		return databaseError(err)
	}
	if err := commit(tx); err != nil {
		return err
	}
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: &runnerv1.JobOffer{JobId: row.JobID, LeaseGeneration: leaseGeneration, TaskPacketJson: string(encoded)}}}
	s.enqueue(runnerID, message)
	// A failed enqueue (the runner disconnected between SelectEligibleReviewRunner
	// and here) leaves the job at 'offered' with nothing to answer it. Unlike
	// dispatchReviewerJob's own equivalent case, this is not reclaimed by a
	// dedicated review-scoped sweep: it falls to the generic, role='developer'-
	// scoped reclaimUnansweredOffers/reclaimExpiredLeases (recovery.go), which
	// ends the run and returns its issue to the queue for a fresh attempt --
	// coarser than a targeted redispatch, but not a stranding bug, and simplest
	// given how rarely a runner disconnects in the narrow window between
	// selection and enqueue.
	return nil
}

// pipelineRepairEligible is repairEligible's counterpart for a run whose
// developer execution's own deterministic pipeline failed (#352): the
// project must have opted into EnableRepairLoop -- the same toggle #354's
// AI-review-triggered repair already uses, so a project need not configure
// two separate opt-ins for "retry a rejected review" and "retry a failed
// pipeline" -- and this run must not have already spent its bound of
// pipeline_repair_attempts, a column of its own so the two triggers never
// share (and therefore never silently halve) one budget.
func pipelineRepairEligible(ctx context.Context, queries *db.Queries, workflowID string) (bool, error) {
	row, err := queries.GetPipelineRepairState(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(row.Configuration, &config); err != nil {
		return false, configurationError(err)
	}
	return config.EnableRepairLoop && row.PipelineRepairAttempts < maxRepairAttempts, nil
}

// pipelineFailedOrBlock is persistExecutionEvent's (inline, right after its
// own transaction committing the run at StatusPipelineFailed) and
// applyStrandedPipelineDecision's (recovery.go sweep) shared decision for a
// developer execution whose agent succeeded but whose project's own
// deterministic pipeline -- PROJECT.md's "completion gate" -- failed a
// required command: repair it, bounded and informed by the failing command's
// own output, if the project opted into EnableRepairLoop and the run has
// pipeline_repair_attempts left; otherwise end the run at StatusBlocked
// exactly as V1 always has, with reason as the recorded cause.
//
// Unlike repairOrBlock, no separate execution has to run before this
// decision can be made -- the pipeline's own exit codes are the verdict, and
// persistExecutionEvent already has them the moment the developer's terminal
// event arrives -- so there is no equivalent of StatusWaitingAiReview's wait
// for more information here; StatusPipelineFailed exists only to hand the
// already-known verdict from the transaction that recorded it to this
// decision, exactly the width of persistExecutionEvent's own after-commit
// call.
func (s *Core) pipelineFailedOrBlock(ctx context.Context, workflowID, reason string) error {
	eligible, err := pipelineRepairEligible(ctx, s.queries, workflowID)
	if err != nil {
		return err
	}
	if eligible {
		return s.dispatchPipelineRepairJob(ctx, workflowID)
	}
	return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "pipeline.failed", reason)
}

// pipelineRepairExecutionID is repairExecutionID's counterpart for a
// pipeline-triggered repair attempt, namespaced distinctly ("-pipeline-repair-"
// rather than "-repair-") so a run that spends attempts from both
// ci_repair_attempts and pipeline_repair_attempts over its life -- an AI-review
// rejection repaired once, whose repaired attempt's own pipeline then fails --
// never reuses an execution ID either counter's own attempt 1 already retired.
func pipelineRepairExecutionID(jobID string, attempt int32) string {
	return fmt.Sprintf("%s-pipeline-repair-%d", jobID, attempt)
}

// dispatchPipelineRepairJob is dispatchRepairJob's counterpart for a
// pipeline-triggered repair, called by pipelineFailedOrBlock once it has
// decided the run is eligible. Reuses repairPacket (the developer-role shape
// both triggers dispatch) with pipelineRepairObjective and the failing
// command recorded in blocking_reason as this attempt's one finding, and
// reopens the job from role='developer'/status='failed' -- the shape a
// pipeline failure's own terminal event leaves the job in -- rather than
// ReopenJobForRepair's role='reviewer'/status='completed', since a pipeline
// failure never routed the job through a reviewer role at all.
//
// Every step is a guarded, idempotent no-op if it finds nothing to do, the
// same shape dispatchRepairJob and dispatchReviewerJob already established.
func (s *Core) dispatchPipelineRepairJob(ctx context.Context, workflowID string) error {
	runners := s.connectedRunners()
	if len(runners) == 0 {
		return nil // applyStrandedPipelineDecision (recovery.go) retries once a runner connects
	}
	row, err := s.queries.GetPipelineRepairDispatchWorkflow(ctx, db.GetPipelineRepairDispatchWorkflowParams{ID: workflowID, MaxRepairAttempts: maxRepairAttempts})
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
		return nil // applyStrandedPipelineDecision retries once an eligible runner is free
	}
	if err != nil {
		return databaseError(err)
	}
	attempt := row.PipelineRepairAttempts + 1
	findings := []string{}
	if row.BlockingReason != "" {
		findings = []string{row.BlockingReason}
	}
	pipeline, err := pipelineStepsForPacket(ctx, s.queries, row.ProjectID)
	if err != nil {
		return err
	}
	packet, err := repairPacket(row.JobID, pipelineRepairExecutionID(row.JobID, attempt), attempt, row.ProjectID, row.ExternalID, row.Title, row.Body, row.RepositoryMode, row.RepositoryUrl, row.LocalRepositoryPath, row.DefaultBranch, row.BranchName, config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds), pipelineRepairObjective, pipeline, findings)
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
	affected, err := queries.MarkWorkflowPipelineRepairing(ctx, db.MarkWorkflowPipelineRepairingParams{ID: workflowID, MaxRepairAttempts: maxRepairAttempts})
	if err != nil {
		return databaseError(err)
	}
	if affected != 1 {
		return nil // raced away from 'pipeline_failed', or the bound was already reached
	}
	leaseGeneration, err := queries.ReopenJobForPipelineRepair(ctx, db.ReopenJobForPipelineRepairParams{RunnerID: runnerID, ID: row.JobID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // raced with another dispatch attempt for this same job
	}
	if err != nil {
		return databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: idgen.NewID(), JobID: row.JobID, RunnerID: runnerID}); err != nil {
		return databaseError(err)
	}
	if err := queries.CreateWorkflowEvent(ctx, db.CreateWorkflowEventParams{
		WorkflowRunID: workflowID, EventType: "workflow_transition", Severity: "info",
		Column4: []byte(fmt.Sprintf(`{"reason":"dispatching pipeline repair attempt %d of %d, informed by the failing command's own output"}`, attempt, maxRepairAttempts)),
	}); err != nil {
		return databaseError(err)
	}
	if err := commit(tx); err != nil {
		return err
	}
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: &runnerv1.JobOffer{JobId: row.JobID, LeaseGeneration: leaseGeneration, TaskPacketJson: string(encoded)}}}
	s.enqueue(runnerID, message)
	// A failed enqueue leaves the job at 'offered' with nothing to answer it,
	// reclaimed the same generic way dispatchRepairJob's own equivalent case is
	// (see its own trailing comment): reclaimUnansweredOffers/reclaimExpiredLeases
	// (recovery.go), scoped by role='developer' with no trigger-specific
	// distinction needed.
	return nil
}
