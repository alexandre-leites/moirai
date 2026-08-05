package server

import (
	"context"
	"encoding/json"
	"errors"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Phase-dispatch: workflow-run state machine topology
//
// persistExecutionEvent below is the single writer that moves a workflow run
// between the Status values defined in status.go, in response to a runner's
// ExecutionEvent for the one job a run holds. This block documents every
// transition edge it (directly, or via the after-commit calls it makes) is
// responsible for, so a future reader can see the legal moves without
// tracing the whole switch. See each Status constant's own doc comment in
// status.go for the fuller "why"; this is the map.
//
//	offered ──(scheduler dispatches, out of scope here)──> preparing | planning
//
//	planning ──planner "completed"──> planning (unchanged; see plannerCompleted
//	    below) ──dispatchImplementationJob, after commit──> preparing
//	    (re-offers the same job with the developer packet)
//	planning ──planner "failed"/"cancelled"──> ordinary terminal handling,
//	    exactly like a failed/cancelled developer execution (falls into the
//	    `default` case below, keyed on event type, not on job role)
//
//	preparing ──developer "completed", AI review disabled──> delivering
//	    ──deliverWorkflow, after commit──> waiting_github_checks (or sideways
//	    to blocked if opening the PR fails)
//	preparing ──developer "completed", AI review enabled (EnableAiReview)──>
//	    waiting_ai_review ──dispatchReviewerJob, after commit (reopens the
//	    same job for a fresh reviewer execution)
//	preparing ──developer "completed", but the project's own pipeline failed
//	    a required command──> pipeline_failed ──pipelineFailedOrBlock, after
//	    commit──> repairing (if EnableRepairLoop and attempts remain) | blocked
//	preparing ──developer "failed", agent gave a reason (blocked: true)──>
//	    blocked (blockingReason set from the payload)
//	preparing ──developer "failed", no reason, or "cancelled"──> failed |
//	    cancelled (matches the event type verbatim)
//
//	waiting_ai_review ──reviewer "completed", approving verdict──>
//	    handleReviewCompletion (after commit) ──deliverWorkflow──>
//	    waiting_github_checks (same path as AI-review-disabled above)
//	waiting_ai_review ──reviewer "completed", rejecting verdict──>
//	    handleReviewCompletion (after commit)──> repairing (if EnableRepairLoop
//	    and ci_repair_attempts remain) | blocked
//	waiting_ai_review ──reviewer "failed"/"cancelled" (crashed without a
//	    verdict)──> blocked outright (never repaired: no developer mistake
//	    exists yet to repair against)
//
//	pipeline_failed ──pipelineFailedOrBlock, after commit──> repairing | blocked
//	repairing ──repaired developer execution's own "completed"/"failed"
//	    event──> flows back through the exact same preparing-shaped branches
//	    above (persistExecutionEvent does not consult the run's current
//	    status, only job role and event type) until pipeline_repair_attempts
//	    or ci_repair_attempts is exhausted and the matching *OrBlock ends the
//	    run at blocked for good
//
//	waiting_github_checks ──observeWorkflow (out of scope here)──>
//	    completed | waiting_human | blocked
//	waiting_human ──SubmitHumanDecision (out of scope here)──>
//	    waiting_github_checks | blocked
//
// completed, failed, blocked and cancelled are the run's terminal statuses
// (terminalStatuses in status.go); nothing in this file writes a run away
// from them. delivering, waiting_ai_review, pipeline_failed and repairing are
// deliberately excluded from that terminal set even though this file is what
// sets them: each is a brief hand-off to a decision made after commit, not a
// resting state, exactly as their own doc comments in status.go describe.
//
// The project lock (app.project_locks) is released only when a run lands
// on a non-terminal-but-done edge outside delivering/waiting_ai_review/
// pipeline_failed -- see the DeleteProjectLockByWorkflow call below and its
// comment for why "failed"/"blocked"/"cancelled" release it while the other
// three don't.

func (s *Core) persistExecutionEvent(ctx context.Context, runnerID string, event *runnerv1.ExecutionEvent) error {
	if !idgen.ValidID(event.GetJobId()) || event.GetLeaseGeneration() < 1 || event.GetEventSequence() < 1 || !validEventType(event.GetType()) || !json.Valid([]byte(event.GetPayloadJson())) {
		return status.Error(codes.InvalidArgument, "execution event is invalid")
	}
	// payload_json is agent-supplied end to end: the runner forwards
	// payload["result"] (the agent's own result document) verbatim, so nothing
	// here may reject it outright -- losing a terminal event strands the run
	// and its project lock with no operator recourse. json.Valid above only
	// proves it parses; it does not prove Postgres can store it. jsonb's
	// underlying text type has no representation for an embedded NUL or an
	// unpaired UTF-16 surrogate half, both legal inside a `\uXXXX` escape as
	// far as encoding/json is concerned, and rejects them with "unsupported
	// Unicode escape sequence" / "invalid input syntax for type json",
	// aborting the whole INSERT. sanitizeEventPayload neutralises both before
	// the payload reaches CreateWorkflowEvent's ::jsonb argument (and before
	// agentBlockReason below, since blocking_reason is a plain text column
	// with the same restriction).
	payloadJSON := sanitizeEventPayload(event.GetPayloadJson())
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	recorded, err := queries.RecordJobExecutionEvent(ctx, db.RecordJobExecutionEventParams{
		ID: event.GetJobId(), RunnerID: runnerID, LeaseGeneration: event.GetLeaseGeneration(),
		EventSequence: event.GetEventSequence(), EventType: event.GetType(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.FailedPrecondition, "execution event is stale")
	}
	if err != nil {
		return databaseError(err)
	}
	workflowID, jobRole := recorded.WorkflowRunID, recorded.Role
	// A reviewer's own "completed" event is not an ordinary developer success:
	// its verdict, not its mere existence, decides what happens next, and that
	// decision (handleReviewCompletion, review.go) needs the payload this
	// transaction is about to commit, so it runs after commit, the same
	// after-commit shape deliverWorkflow already runs in below. A planner's own
	// "completed" event (#351) is the same shape again: the plan it produced,
	// not its mere existence, is what dispatchImplementationJob needs, so it
	// too runs after commit rather than being folded into the generic terminal
	// handling below.
	reviewerCompleted := jobRole == jobRoleReviewer && event.GetType() == "completed"
	plannerCompleted := jobRole == jobRolePlanner && event.GetType() == "completed"
	// The event type is stored unprefixed because it is a vocabulary shared with
	// the console, which switches on bare "log", "started", "failed" and friends
	// to build the timeline and the agent log pane. Writing "runner.log" here
	// left every one of those rows falling through to the default branch.
	if err := queries.CreateWorkflowEvent(ctx, db.CreateWorkflowEventParams{
		WorkflowRunID: workflowID, EventType: event.GetType(), Severity: eventSeverity(event.GetType()), Column4: []byte(payloadJSON),
	}); err != nil {
		return databaseError(err)
	}
	// enteringReview is set inside the terminalEvent branch below, for a
	// developer's "completed" event on a project with EnableAiReview set, and
	// read again after commit to decide dispatchReviewerJob vs deliverWorkflow
	// -- one aiReviewEnabled read inside the transaction, not two. pipelineFailed
	// is its counterpart for a developer (or repair) execution whose agent
	// succeeded but whose own deterministic pipeline then failed a required
	// command (#352): read again after commit to hand the decision to
	// pipelineFailedOrBlock, the same shape enteringReview hands off to
	// dispatchReviewerJob.
	var enteringReview, pipelineFailed bool
	var pipelineFailedReason string
	if terminalEvent(event.GetType()) {
		// The event type is the shared vocabulary and is stored as it arrived;
		// the run's own terminal status is derived from it. An agent that
		// stopped deliberately and said why reports a `failed` event whose
		// payload is marked `blocked: true` (runner/README.md, "An
		// agent-reported block is not a crash"), and ending that run as
		// `failed` with an empty blocking_reason made a stated, actionable stop
		// indistinguishable from an anonymous crash -- and kept it out of the
		// console's needs-attention triage, which is waiting_human ∪ blocked.
		//
		// blocking_reason and terminal_reason are left untouched when no reason
		// was derived, which is every path but this one, so an ordinary failure
		// writes exactly the columns it wrote before. No competing value should
		// exist to preserve — the other two writers of those columns terminate
		// the run and fence its job, and this statement is only reached by an
		// event that passed the fence — so the COALESCE is a backstop against
		// that reasoning changing, not a case anything is known to hit.
		// event.GetType() is one of "completed", "failed" or "cancelled" here
		// (terminalEvent already fenced anything else), which is the
		// runner-event vocabulary Status shares for "failed" and "cancelled" --
		// but not for "completed", which the run's own status column no longer
		// reuses. A "completed" event means the agent succeeded, not that
		// delivery (opening/merging the pull request) has, so the run moves to
		// StatusDelivering instead: see its doc comment in status.go for why
		// conflating the two under one 'completed' value was the bug #267
		// fixed. The event row above still records the runner's own
		// "completed", since that is the separate, shared vocabulary the
		// console's event timeline switches on.
		//
		// A reviewer's own "completed" event is handled entirely separately,
		// below and after commit (handleReviewCompletion): its status
		// transition depends on a verdict this switch has no business parsing
		// mid-transaction, and the run must stay at StatusWaitingAiReview,
		// untouched, until that decision lands -- exactly the reasoning
		// StatusDelivering's own doc comment already gives for why a run
		// holding an in-progress status must not be mistaken for one at rest.
		switch {
		case reviewerCompleted:
			// Touches nothing but updated_at (same status in, same status
			// out), so resumeStrandedReviewVerdicts' age bound (recovery.go's
			// strandedReviewVerdict) starts counting from this commit rather
			// than from whenever the review was first dispatched -- a review
			// execution can run for as long as its own timeout allows, and
			// without this the sweep would race handleReviewCompletion's own
			// inline call below on every single review.
			if err := queries.SetWorkflowTerminalStatus(ctx, db.SetWorkflowTerminalStatusParams{
				ID: workflowID, Status: StatusWaitingAiReview.String(), Column3: "",
			}); err != nil {
				return databaseError(err)
			}
		case plannerCompleted:
			// Touches nothing but updated_at, the same reasoning
			// reviewerCompleted's case gives: the run stays at StatusPlanning
			// until dispatchImplementationJob (called after commit, below)
			// reopens the same job for its developer execution and this
			// status is overwritten by that job's own acceptOffer call into
			// SetWorkflowPreparing.
			if err := queries.SetWorkflowTerminalStatus(ctx, db.SetWorkflowTerminalStatusParams{
				ID: workflowID, Status: StatusPlanning.String(), Column3: "",
			}); err != nil {
				return databaseError(err)
			}
		default:
			runStatus, blockingReason := Status(event.GetType()), ""
			switch event.GetType() {
			case "completed":
				runStatus = StatusDelivering
				enabled, cfgErr := aiReviewEnabled(ctx, queries, workflowID)
				if cfgErr != nil {
					return cfgErr
				}
				if enabled {
					runStatus = StatusWaitingAiReview
					enteringReview = true
				}
			case "failed":
				if jobRole == jobRoleReviewer {
					// A reviewer execution that crashed or was cancelled
					// without producing a verdict is not an ordinary agent
					// failure to fold into agentBlockReason's developer-shaped
					// account: block outright with a reason distinct from it.
					// This is deliberately terminal rather than routed through
					// repairOrBlock (#354, repair.go): the reviewer itself never
					// reached a verdict, so there is no developer mistake to
					// repair against, only an infrastructure-side failure of the
					// review step -- distinct in kind from a review that ran to
					// completion and rejected the change, which is what the
					// repair loop consumes.
					runStatus, blockingReason = StatusBlocked, "independent AI review execution failed"
				} else if reason, failed := pipelineFailureReason(payloadJSON); failed {
					// A developer (or repair) execution's own agent completed --
					// pipelineFailureReason can only ever find a pipelineResults
					// entry when dispatch.go's own guard ("executeErr == nil &&
					// result.Status == completed") let the pipeline run at all --
					// but the project's configured pipeline then failed a required
					// command. This is deliberately its own status rather than
					// StatusBlocked outright: pipelineFailedOrBlock (repair.go,
					// called after commit below) still has to decide whether the
					// project's EnableRepairLoop opt-in and remaining
					// pipeline_repair_attempts make this repairable.
					runStatus, blockingReason = StatusPipelineFailed, reason
					pipelineFailed, pipelineFailedReason = true, reason
				} else if reason, blocked := agentBlockReason(payloadJSON); blocked {
					runStatus, blockingReason = StatusBlocked, reason
				}
			case "cancelled":
				if jobRole == jobRoleReviewer {
					runStatus, blockingReason = StatusBlocked, "independent AI review execution was cancelled"
				}
			}
			if err := queries.SetWorkflowTerminalStatus(ctx, db.SetWorkflowTerminalStatusParams{
				ID: workflowID, Status: runStatus.String(), Column3: blockingReason,
			}); err != nil {
				return databaseError(err)
			}
			if runStatus != StatusDelivering && runStatus != StatusWaitingAiReview && runStatus != StatusPipelineFailed {
				if err := queries.DeleteProjectLockByWorkflow(ctx, workflowID); err != nil {
					return databaseError(err)
				}
				// V1 has no automatic retry: a run that lands on 'failed' or
				// 'blocked' here is excluded from the scheduler's candidate set
				// (ListQueueEntries, ClaimSchedulableIssue) by its own status,
				// via the app.workflow_runs join those queries run, until
				// RetryWorkflow supersedes it. Nothing needs writing to the issue
				// itself any more -- see #268. A 'cancelled' event lands on a
				// status that join does not exclude, so it is picked up again
				// with no operator action, the same as an operator-cancelled or
				// unanswered-offer run.
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	switch {
	case reviewerCompleted:
		return s.handleReviewCompletion(ctx, workflowID, payloadJSON)
	case plannerCompleted:
		return s.dispatchImplementationJob(ctx, workflowID, event.GetJobId(), payloadJSON)
	case enteringReview:
		return s.dispatchReviewerJob(ctx, workflowID)
	case pipelineFailed:
		return s.pipelineFailedOrBlock(ctx, workflowID, pipelineFailedReason)
	case event.GetType() == "completed":
		return s.deliverWorkflow(ctx, workflowID)
	}
	return nil
}
