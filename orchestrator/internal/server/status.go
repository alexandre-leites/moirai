package server

import "slices"

// Status is the vocabulary written to app.workflow_runs.status. Before this
// type existed the vocabulary was spelled out as raw SQL string literals
// scattered across server.go, delivery.go and recovery.go, with no compiler
// check that a new writer used a value any reader recognised -- see #265.
// These seven values are every one those files write today; 021_workflow_run_
// status_check.sql enforces the same set as a database CHECK, so the two
// copies cannot drift silently again.
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
	// StatusWaitingGithubChecks is set once a pull request has been opened
	// and the run is waiting for GitHub's checks to report a result.
	StatusWaitingGithubChecks Status = "waiting_github_checks"
	// StatusCompleted marks a run whose agent execution succeeded. It is
	// reached twice: once when the agent finishes (persistExecutionEvent),
	// and again once GitHub confirms the merge (observeWorkflow) -- the run
	// passes back through 'waiting_github_checks' in between. See
	// genuinelyTerminalStatuses for why this status is not treated as
	// terminal everywhere.
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

// quoted renders a single Status as a SQL literal ('offered'), for the
// queries below that spell a status directly into the query text instead of
// binding it as a parameter.
func (s Status) quoted() string { return "'" + string(s) + "'" }

// qOffered, qPreparing etc. are the query-text form of each status, named so
// a query that spells 'offered' now says qOffered and fails to compile if the
// constant it comes from is ever renamed or removed -- the raw string
// literals server.go, delivery.go and recovery.go used to write by hand.
var (
	qOffered             = StatusOffered.quoted()
	qPreparing           = StatusPreparing.quoted()
	qWaitingGithubChecks = StatusWaitingGithubChecks.quoted()
	qCompleted           = StatusCompleted.quoted()
	qBlocked             = StatusBlocked.quoted()
	qCancelled           = StatusCancelled.quoted()
)

// knownStatuses backs ParseStatus. It is built from the constants above
// rather than restated so the parser and the vocabulary can never disagree.
var knownStatuses = map[Status]bool{
	StatusOffered:             true,
	StatusPreparing:           true,
	StatusWaitingGithubChecks: true,
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
// StatusCompleted is on this list even though terminateWorkflow's own guard
// (see genuinelyTerminalStatuses) deliberately still allows a completed run
// to move to 'blocked': the two predicates answer different questions.
// terminalStatuses answers "is this run done, for a dashboard counting active
// work" -- yes, a completed run is done. genuinelyTerminalStatuses answers
// "can external delivery still fail this run" -- yes, because 'completed'
// only means the agent succeeded, and the pull request it still has to open
// or merge can fail after that. That asymmetry is what lets a delivery
// failure turn a 'completed' run into 'blocked' without racing a second
// delivery attempt for the same run (see resumeStrandedDeliveries in
// recovery.go).
var terminalStatuses = []Status{StatusCompleted, StatusFailed, StatusBlocked, StatusCancelled}

// terminalStatusList renders them as the SQL literal list `'a','b'`. Built
// from the constants above, never from input.
var terminalStatusList = sqlStatusList(terminalStatuses)

func terminalStatus(state string) bool {
	status, ok := ParseStatus(state)
	return ok && slices.Contains(terminalStatuses, status)
}

// genuinelyTerminalStatuses are the three statuses no code path ever moves a
// run away from once reached -- deliberately not the same set as
// terminalStatuses, which also lists StatusCompleted.
//
// StatusCompleted is left out here on purpose: it means the agent execution
// succeeded, not that delivery did, and two different writers still move a
// run away from it -- deliverWorkflow forward to 'waiting_github_checks', and
// blockExternal (via terminateWorkflow) sideways to 'blocked' when opening or
// merging the pull request fails. Excluding StatusCompleted from
// terminateWorkflow's own guard is what lets that delivery failure still
// land; including it here would leave a run whose delivery failed reporting
// success forever, and RetryWorkflow would never make it eligible again
// either. See terminalStatuses for the (different, and equally intentional)
// set that does include it.
var genuinelyTerminalStatuses = []Status{StatusFailed, StatusBlocked, StatusCancelled}

var genuinelyTerminalStatusList = sqlStatusList(genuinelyTerminalStatuses)

func genuinelyTerminalStatus(state string) bool {
	status, ok := ParseStatus(state)
	return ok && slices.Contains(genuinelyTerminalStatuses, status)
}

// sqlStatusList renders a status set as the SQL literal list `'a','b'` for
// splicing into a query's IN (...) / NOT IN (...) clause. Every caller passes
// a package-level slice built from the Status constants, never from input.
func sqlStatusList(statuses []Status) string {
	list := ""
	for i, status := range statuses {
		if i > 0 {
			list += ","
		}
		list += status.quoted()
	}
	return list
}
