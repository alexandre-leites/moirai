package server

import "slices"

// Status is the vocabulary written to app.workflow_runs.status. Before this
// type existed the vocabulary was spelled out as raw SQL string literals
// scattered across server.go, delivery.go and recovery.go, with no compiler
// check that a new writer used a value any reader recognised -- see #265.
// These eight values are every one those files write today; 021_workflow_run_
// status_check.sql (extended by 022_workflow_run_delivering_status.sql to add
// 'delivering') enforces the same set as a database CHECK, so the two copies
// cannot drift silently again.
//
// 'running' is deliberately not here: it is a status app.jobs uses for an
// execution in flight, but no code path ever writes it to a workflow run.
type Status string

const (
	// StatusOffered is a run's initial status: the scheduler has picked an
	// issue and offered its job to a runner, and nothing has answered yet.
	StatusOffered Status = "offered"
	// StatusPreparing is set once a runner accepts its job offer, and holds
	// for the whole execution -- there is no separate "running" status for a
	// workflow run, only for the job underneath it.
	StatusPreparing Status = "preparing"
	// StatusPlanning is set once a runner accepts a planning job offer (see
	// projectConfig.RequirePlanning), and holds for the whole planner
	// execution -- the same relationship StatusPreparing has to the developer
	// execution that follows it. ScheduleOnce dispatches a planner-role
	// packet instead of the developer packet for an opted-in project; once
	// that execution reports "completed", persistExecutionEvent records the
	// plan and re-offers the same job (app.jobs.workflow_run_id stays UNIQUE
	// -- this is a second offer/accept/lease cycle for the one job the
	// workflow already has, not a second job) with the developer packet,
	// carrying the plan forward as its Plan context. A planner execution that
	// fails, is blocked, or is cancelled falls through to the ordinary
	// terminal handling below, exactly like a failed developer execution.
	StatusPlanning Status = "planning"
	// StatusWaitingGithubChecks is set once a pull request has been opened
	// and the run is waiting for GitHub's checks to report a result.
	StatusWaitingGithubChecks Status = "waiting_github_checks"
	// StatusDelivering marks a run whose agent execution succeeded and whose
	// pull request is being opened. persistExecutionEvent sets it (in place of
	// StatusCompleted -- see #267) for the runner's 'completed' event, and it
	// deliberately keeps the project lock while it holds this status: the run
	// still has to hand its work to GitHub, and a crash before that finishes
	// must not look like nothing is holding the project.
	//
	// Before this status existed 'completed' was written at this point too,
	// which meant "the agent succeeded" and "the pull request merged" were the
	// same value and could only be told apart by whether a project_locks row
	// still happened to exist -- see resumeStrandedDeliveries (recovery.go)
	// for what that ambiguity cost. A run leaves 'delivering' for
	// 'waiting_github_checks' once deliverWorkflow opens the pull request, or
	// sideways to 'blocked' if opening it fails (blockExternal).
	StatusDelivering Status = "delivering"
	// StatusWaitingHuman marks a run whose GitHub checks are already green and
	// whose project opted into the human-approval gate (projectConfig's
	// RequireHumanApproval): observeWorkflow stops here instead of merging
	// automatically, and the run holds its project lock exactly like
	// StatusWaitingGithubChecks -- it is still doing work, just waiting on a
	// person instead of on GitHub. SubmitHumanDecision is what moves a run away
	// from this status: "approved" sends it back to StatusWaitingGithubChecks
	// so the same tested merge path runs again (checks are already green, so
	// the next observer tick, or SubmitHumanDecision's own best-effort
	// immediate call, merges it straight away); "changes_requested" sends it to
	// StatusBlocked with the reviewer's comment as the reason, the same
	// terminal shape an agent's own declared block uses.
	StatusWaitingHuman Status = "waiting_human"
	// StatusWaitingAiReview marks a run whose developer execution reported
	// success and whose project opted into the independent-AI-review gate
	// (projectConfig's EnableAiReview): persistExecutionEvent sets it in place
	// of StatusDelivering for that project, and dispatchReviewerJob
	// (review.go) reopens the run's one job for a second, independent
	// execution -- a fresh reviewer session with no access to the developer's
	// own conversation, per AGENTS.md's "use fresh context for independent AI
	// review". The run holds its project lock across this status exactly like
	// StatusWaitingHuman: it is still doing work, just work a second agent
	// is doing instead of the first.
	//
	// handleReviewCompletion (review.go) is what moves a run away from this
	// status once the reviewer's own terminal event arrives: an approving
	// verdict hands off to deliverWorkflow, the same path a project with AI
	// review disabled always used; a rejecting verdict (or a reviewer
	// execution that crashed without one) ends the run at StatusBlocked with
	// the verdict recorded in app.ai_reviews -- the same terminal shape a
	// failed deterministic pipeline check would use, and the signal #354's
	// repair loop is meant to consume.
	StatusWaitingAiReview Status = "waiting_ai_review"
	// StatusRepairing marks a run whose independent AI review rejected the
	// developer's own attempt, or whose own deterministic pipeline failed a
	// required command (#352), and whose project opted into the repair loop
	// (projectConfig's EnableRepairLoop): repairOrBlock/pipelineFailedOrBlock
	// (repair.go) set it, via dispatchRepairJob/dispatchPipelineRepairJob, in
	// place of terminating the run at StatusBlocked -- the same
	// bounded escape hatch #354 adds so a rejection or a failed pipeline is not
	// always the end of the run, spent from ci_repair_attempts or
	// pipeline_repair_attempts respectively (two independent counters sharing
	// one bound and one opt-in, so a project need not configure "retry a
	// rejected review" and "retry a failed pipeline" separately). The run holds
	// its project lock across this status exactly like StatusWaitingAiReview or
	// StatusPipelineFailed: it is still doing work, a second (or third)
	// developer attempt informed by the reviewer's findings or the pipeline's
	// own failing command instead of a blind re-run.
	//
	// A repaired attempt's own "completed" event is not read any differently
	// than the original developer attempt's: persistExecutionEvent's switch on
	// job role and event type does not consult the run's current status, so the
	// repaired attempt flows back through the exact same
	// StatusDelivering/StatusWaitingAiReview/StatusPipelineFailed branch the
	// first attempt did, until its own attempts column exhausts the bound and
	// the matching *OrBlock falls through to StatusBlocked for good.
	StatusRepairing Status = "repairing"
	// StatusPipelineFailed marks a run whose developer (or repair) execution's
	// agent succeeded but whose project's own configured local pipeline --
	// PROJECT.md's "deterministic completion gate" -- failed a required
	// command (#352): persistExecutionEvent sets it in place of StatusDelivering
	// (or StatusWaitingAiReview) for that event, and pipelineFailedOrBlock
	// (repair.go) decides right after commit whether to repair the run
	// (bounded by pipeline_repair_attempts, the same EnableRepairLoop opt-in
	// #354's AI-review repair already uses) or end it at StatusBlocked. The run
	// holds its project lock across this status exactly like
	// StatusWaitingAiReview: the pipeline's own verdict already exists (unlike
	// AI review, no second execution has to run before there is one to act
	// on), so this status is a brief hand-off to the repair-or-block decision,
	// not a wait for more information.
	StatusPipelineFailed Status = "pipeline_failed"
	// StatusCompleted marks a run whose pull request GitHub has confirmed
	// merged (observeWorkflow) -- the true terminal "done" state. See
	// StatusDelivering for the status this run passed through on the way
	// here, and genuinelyTerminalStatuses for why StatusCompleted is still
	// not treated as terminal by the retry/cancel guard even though nothing
	// today writes a run away from it.
	StatusCompleted Status = "completed"
	// StatusFailed marks an agent execution that ended badly and did not
	// declare itself blocked, or a job whose lease lapsed without a runner
	// reporting an outcome.
	StatusFailed Status = "failed"
	// StatusBlocked marks a run an agent deliberately stopped and explained,
	// or one external delivery (opening/merging the pull request) could not
	// complete.
	StatusBlocked Status = "blocked"
	// StatusCancelled marks a run an operator stopped, or one whose job
	// offer nobody answered.
	StatusCancelled Status = "cancelled"
)

// String satisfies fmt.Stringer so a Status prints as its bare value in log
// lines and test failure messages, the same as the string literals it
// replaces did.
func (s Status) String() string { return string(s) }

// knownStatuses backs ParseStatus. It is built from the constants above
// rather than restated so the parser and the vocabulary can never disagree.
var knownStatuses = map[Status]bool{
	StatusOffered:             true,
	StatusPreparing:           true,
	StatusPlanning:            true,
	StatusWaitingGithubChecks: true,
	StatusWaitingHuman:        true,
	StatusWaitingAiReview:     true,
	StatusRepairing:           true,
	StatusPipelineFailed:      true,
	StatusDelivering:          true,
	StatusCompleted:           true,
	StatusFailed:              true,
	StatusBlocked:             true,
	StatusCancelled:           true,
}

// ParseStatus validates a value read back from app.workflow_runs.status (or
// received over the wire) against the known vocabulary. The database CHECK
// constraint (021_workflow_run_status_check.sql) already refuses anything
// else at write time, so a false here on a row read from this database means
// the constraint and this list have drifted, not that the row is unusual.
func ParseStatus(value string) (Status, bool) {
	status := Status(value)
	return status, knownStatuses[status]
}

// terminalStatuses are the workflow-run statuses a run never leaves under
// its own status column's ordinary meaning. Both the Go predicate below and
// the SQL the active-workflow gauge counts with (020_metrics_indexes.sql's
// workflow_runs_active_idx) are derived from this one list: they were
// independent copies, and a fifth terminal status added to one and not the
// other would have left the gauge counting finished work as active, with
// nothing to catch it.
//
// StatusDelivering is deliberately absent: a run holding it is still doing
// active work (opening a pull request), the same reason 'offered', 'preparing',
// 'planning', 'waiting_github_checks', 'waiting_human', 'waiting_ai_review',
// 'pipeline_failed' and 'repairing' are absent -- a run running its planner execution holds the project lock
// exactly as actively as one running its developer execution, a run waiting
// on a person to decide, or dispatching a bounded repair attempt, is exactly
// as active as one waiting on GitHub's checks, and must keep its project lock
// and count toward moirai_active_workflows the same
// way. See genuinelyTerminalStatuses for the narrower set terminateWorkflow's
// own guard uses, and StatusDelivering's doc comment above for why 'completed'
// and 'delivering' no longer share one meaning the way this list's single
// StatusCompleted entry once had to cover.
var terminalStatuses = []Status{StatusCompleted, StatusFailed, StatusBlocked, StatusCancelled}

func terminalStatus(state string) bool {
	status, ok := ParseStatus(state)
	return ok && slices.Contains(terminalStatuses, status)
}

// genuinelyTerminalStatuses are the three statuses no code path ever moves a
// run away from once reached -- deliberately not the same set as
// terminalStatuses, which also lists StatusCompleted.
//
// StatusCompleted is left out here on purpose, even though (since #267 split
// 'completed' from StatusDelivering) nothing today actually moves a run away
// from it once GitHub confirms the merge: RetryWorkflow's guard is "current
// status is genuinely terminal", and a completed run is done, not retryable,
// so it must read as *not* genuinely terminal here to keep failing that guard
// with "not retryable" rather than being treated as eligible for a fresh
// attempt. StatusDelivering is excluded from this list for the opposite,
// ordinary reason: a run holding it is still in flight, and blockExternal (via
// terminateWorkflow) sideways-moves it to 'blocked' when opening or merging
// the pull request fails -- see StatusDelivering's doc comment. See
// terminalStatuses for the (different, and equally intentional) set that does
// include StatusCompleted.
var genuinelyTerminalStatuses = []Status{StatusFailed, StatusBlocked, StatusCancelled}

func genuinelyTerminalStatus(state string) bool {
	status, ok := ParseStatus(state)
	return ok && slices.Contains(genuinelyTerminalStatuses, status)
}
