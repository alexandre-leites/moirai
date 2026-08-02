package server

import (
	"context"
	"errors"
)

// abandonedLease is the one description of a job whose runner stopped
// reporting, used for the job's recovery reason, the run's terminal reason and
// the event payload alike.
const abandonedLease = "runner stopped renewing its lease before reporting an outcome"

// staleRunner is how long a runner may go without a heartbeat before it is
// recorded offline. Runners heartbeat every 10 seconds by default, so this is
// several missed beats rather than a single slow one.
const staleRunner = "90 seconds"

// unansweredOffer is how long an offer may sit unanswered before the job behind
// it is reclaimed. The runner answers immediately in the normal case; this
// covers one that took the offer and died before accepting it.
const unansweredOffer = "5 minutes"

// unansweredOfferReason describes that outcome wherever it is recorded.
const unansweredOfferReason = "runner never answered the job offer"

// strandedDelivery is how long a run may sit at 'completed' holding its project
// lock before the sweep assumes the delivery that was meant to follow is never
// going to finish.
const strandedDelivery = "5 minutes"

// abandonedChecks bounds the wait for GitHub checks. Nothing else ends that
// wait: a repository whose checks never report — no CI configured, a workflow
// that never queues — would otherwise hold its project lock forever, which is
// the cost of correctly refusing to read "no checks" as success.
const abandonedChecks = "6 hours"

// RecoverOnce reconciles state that no live request will ever come back to
// finish. Every guard in this package is written as `lease_expires_at>now()` or
// `status='waiting_github_checks'`, which correctly refuses stale work but
// leaves the stale rows themselves untouched. A project lock is held by exactly
// one workflow run at a time, so any run that stops making progress while
// holding one takes its whole project down with it until something clears it —
// and a runner killed mid-job, or an orchestrator restarted between committing
// a completion and delivering it, both do exactly that.
//
// The three sweeps are independent, so all three run even if one fails: the two
// that release project locks must not be skipped because a console flag could
// not be written. Running it twice changes nothing the first pass settled.
func (s *Server) RecoverOnce(ctx context.Context) error {
	return errors.Join(
		s.ReconcileDatabaseOnce(ctx),
		s.resumeStrandedDeliveries(ctx),
	)
}

// ReconcileDatabaseOnce is the half of the sweep that only touches the
// database. It is what startup runs, because the other half shells out to
// GitHub once per stranded workflow and the gRPC listener is already open by
// then — a slow GitHub would leave callers queued against a process the
// healthcheck reports as ready.
func (s *Server) ReconcileDatabaseOnce(ctx context.Context) error {
	return errors.Join(
		s.markStaleRunnersOffline(ctx),
		s.reclaimExpiredLeases(ctx),
		s.reclaimUnansweredOffers(ctx),
		s.blockAbandonedChecks(ctx),
	)
}

// reclaimUnansweredOffers releases jobs a runner was offered and never answered.
// An offered job has no lease yet, so the lease sweep cannot see it, and its
// project lock is held from the moment the offer is written — a runner that
// takes an offer and dies before accepting would otherwise wedge that project
// with nothing to reclaim it.
func (s *Server) reclaimUnansweredOffers(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE app.job_offers SET status='expired',responded_at=now() WHERE status='offered' AND job_id IN (SELECT id FROM app.jobs WHERE status='offered' AND offered_at<now()-'`+unansweredOffer+`'::interval)`); err != nil {
		return databaseError(err)
	}
	return s.eachWorkflow(ctx,
		`UPDATE app.jobs SET status='cancelled',finished_at=now(),lease_generation=lease_generation+1,recovery_reason='`+unansweredOfferReason+`' WHERE status='offered' AND offered_at<now()-'`+unansweredOffer+`'::interval RETURNING workflow_run_id::text`,
		func(ctx context.Context, workflowID string) error {
			// The issue is left eligible: nothing ran, so nothing was spent and
			// this work should simply be offered again.
			return s.terminateWorkflow(ctx, workflowID, "cancelled", "cancelled", unansweredOfferReason, false)
		})
}

// blockAbandonedChecks ends a wait for GitHub checks that is never going to
// resolve, so the project can schedule again.
func (s *Server) blockAbandonedChecks(ctx context.Context) error {
	return s.eachWorkflow(ctx,
		`SELECT id::text FROM app.workflow_runs WHERE status='waiting_github_checks' AND updated_at<now()-'`+abandonedChecks+`'::interval ORDER BY updated_at,id LIMIT 20`,
		func(ctx context.Context, workflowID string) error {
			return s.terminateWorkflow(ctx, workflowID, "blocked", "delivery.failed", "GitHub checks did not report a result within "+abandonedChecks, true)
		})
}

// markStaleRunnersOffline clears the online flag for runners that have stopped
// heartbeating. Nothing else ever writes 'offline' outside an operator revoke,
// so after a restart the console otherwise reports every runner that has ever
// connected as online forever.
//
// The predicate is last_seen_at rather than "does this process hold a stream",
// so that a second orchestrator replica does not continually mark the runners
// attached to the first one offline.
func (s *Server) markStaleRunnersOffline(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE app.runners SET status='offline' WHERE status='online' AND (last_seen_at IS NULL OR last_seen_at < now()-$1::interval)`, staleRunner)
	return databaseError(err)
}

// reclaimExpiredLeases fails jobs whose runner stopped renewing. The runner
// cannot rescue them itself: every write path it has is fenced on an unexpired
// lease, so once the lease lapses a reconnecting runner can neither renew it
// nor report the outcome, and the job would stay 'running' forever.
func (s *Server) reclaimExpiredLeases(ctx context.Context) error {
	return s.eachWorkflow(ctx,
		`UPDATE app.jobs SET status='cancelled',finished_at=now(),lease_generation=lease_generation+1,recovery_reason='`+abandonedLease+`' WHERE status IN ('preparing','running') AND lease_expires_at<now() RETURNING workflow_run_id::text`,
		func(ctx context.Context, workflowID string) error {
			return s.terminateWorkflow(ctx, workflowID, "failed", "failed", abandonedLease, true)
		})
}

// resumeStrandedDeliveries re-drives workflows left at 'completed' while still
// holding their project lock. A run reaches that state for the short window
// between the runner's completion being committed and the pull request being
// opened, and the lock is deliberately retained across it. If the process dies
// in that window nothing else looks at the run again: the check observer only
// selects 'waiting_github_checks', and both retry and cancel refuse a run whose
// status is already terminal.
//
// deliverWorkflow finds an existing pull request before creating one, so
// re-driving a delivery that already reached GitHub reuses it.
//
// The age bound is what keeps this from racing a delivery that is still in
// progress. A run matches this predicate for the whole time the inline delivery
// is talking to GitHub, and two concurrent deliveries would leave the loser's
// status update matching no row — reported as a delivery failure, which would
// block a run whose pull request had just been opened successfully.
func (s *Server) resumeStrandedDeliveries(ctx context.Context) error {
	return s.eachWorkflow(ctx, `SELECT wr.id::text FROM app.workflow_runs wr JOIN app.project_locks l ON l.workflow_run_id=wr.id WHERE wr.status='completed' AND wr.updated_at < now()-'`+strandedDelivery+`'::interval ORDER BY wr.updated_at,wr.id LIMIT 20`, s.deliverWorkflow)
}
