package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/loop-engineering/orchestrator/internal/db"
)

// abandonedLease is the one description of a job whose runner stopped
// reporting, used for the job's recovery reason, the run's terminal reason and
// the event payload alike.
const abandonedLease = "runner stopped renewing its lease before reporting an outcome"

// staleRunner is how long a runner may go without a heartbeat before it is
// recorded offline. Runners heartbeat every 10 seconds by default, so this is
// several missed beats rather than a single slow one.
const staleRunner = 90 * time.Second

// unansweredOffer is how long an offer may sit unanswered before the job behind
// it is reclaimed. The runner answers immediately in the normal case; this
// covers one that took the offer and died before accepting it.
const unansweredOffer = 5 * time.Minute

// unansweredOfferReason describes that outcome wherever it is recorded.
const unansweredOfferReason = "runner never answered the job offer"

// strandedDelivery is how long a run may sit at 'delivering' before the sweep
// assumes the delivery that was meant to follow the runner's success is never
// going to finish on its own.
const strandedDelivery = 5 * time.Minute

// strandedReviewDispatch bounds resumeStrandedReviewDispatches: how long a run
// may sit at 'waiting_ai_review' with no reviewer job yet offered before the
// sweep retries the dispatch itself. Short, because dispatchReviewerJob's own
// inline attempt (persistExecutionEvent's post-commit call) only ever fails to
// offer a job for the mundane reason that no runner was connected yet, not
// because anything external is slow.
const strandedReviewDispatch = 2 * time.Minute

// strandedReviewVerdict bounds resumeStrandedReviewVerdicts: how long a
// completed reviewer execution's verdict may go unapplied -- persistExecutionEvent
// committed the terminal event but the process died before
// handleReviewCompletion's follow-on delivery/block decision landed -- before
// the sweep applies it itself. Matches strandedDelivery: an approving verdict
// re-enters deliverWorkflow, the same external-GitHub-call path.
const strandedReviewVerdict = strandedDelivery

// strandedPipelineFailure bounds resumeStrandedPipelineDecisions: how long a
// run may sit at 'pipeline_failed' before the sweep applies
// pipelineFailedOrBlock itself. Short, like strandedReviewDispatch: unlike a
// rejected review, a failed pipeline's verdict already exists the moment
// persistExecutionEvent commits it, so the only reason this decision has not
// landed yet is the same mundane one dispatchReviewerJob's own inline attempt
// can hit -- no runner was connected, or none was eligible, at that moment.
const strandedPipelineFailure = strandedReviewDispatch

// abandonedChecks bounds the wait for GitHub checks. Nothing else ends that
// wait: a repository whose checks never report — no CI configured, a workflow
// that never queues — would otherwise hold its project lock forever, which is
// the cost of correctly refusing to read "no checks" as success.
const abandonedChecks = 6 * time.Hour

// RecoverOnce reconciles state that no live request will ever come back to
// finish. Every guard in this package is written as `lease_expires_at>now()` or
// `status='waiting_github_checks'`, which correctly refuses stale work but
// leaves the stale rows themselves untouched. A project lock is held by exactly
// one workflow run at a time, so any run that stops making progress while
// holding one takes its whole project down with it until something clears it —
// and a runner killed mid-job, or an orchestrator restarted between committing
// the runner's success (status='delivering') and finishing its delivery, both
// do exactly that.
//
// The three sweeps are independent, so all three run even if one fails: the two
// that release project locks must not be skipped because a console flag could
// not be written. Running it twice changes nothing the first pass settled.
func (s *Core) RecoverOnce(ctx context.Context) error {
	return errors.Join(
		s.ReconcileDatabaseOnce(ctx),
		s.resumeStrandedDeliveries(ctx),
		s.resumeStrandedReviewDispatches(ctx),
		s.resumeStrandedReviewVerdicts(ctx),
		s.resumeStrandedPipelineDecisions(ctx),
	)
}

// ReconcileDatabaseOnce is the half of the sweep that only touches the
// database. It is what startup runs, because the other half shells out to
// GitHub once per stranded workflow and the gRPC listener is already open by
// then — a slow GitHub would leave callers queued against a process the
// healthcheck reports as ready.
func (s *Core) ReconcileDatabaseOnce(ctx context.Context) error {
	return errors.Join(
		s.markStaleRunnersOffline(ctx),
		s.reclaimExpiredLeases(ctx),
		s.reclaimUnansweredOffers(ctx),
		s.reclaimExpiredReviewLeases(ctx),
		s.reclaimUnansweredReviewOffers(ctx),
		s.blockAbandonedChecks(ctx),
	)
}

// reclaimUnansweredReviewOffers and reclaimExpiredReviewLeases are the
// reviewer-scoped counterparts of reclaimUnansweredOffers/reclaimExpiredLeases:
// see ReclaimUnansweredReviewOffers/ReclaimExpiredReviewLeases (review.sql)
// for why they reset the job for a redrive instead of cancelling or failing
// the run outright. Both are folded into ReconcileDatabaseOnce so they run on
// the same 30-second cadence, database-only, no GitHub call.
func (s *Core) reclaimUnansweredReviewOffers(ctx context.Context) error {
	workflowIDs, err := s.queries.ReclaimUnansweredReviewOffers(ctx, db.ReclaimUnansweredReviewOffersParams{
		Reason: pgText(unansweredOfferReason), UnansweredOffer: pgInterval(unansweredOffer),
	})
	if err != nil {
		return databaseError(err)
	}
	for _, workflowID := range workflowIDs {
		slog.Info("reviewer offer went unanswered; the job was reset for redispatch", "workflow_run_id", workflowID)
	}
	return nil
}

func (s *Core) reclaimExpiredReviewLeases(ctx context.Context) error {
	workflowIDs, err := s.queries.ReclaimExpiredReviewLeases(ctx, pgText(abandonedLease))
	if err != nil {
		return databaseError(err)
	}
	for _, workflowID := range workflowIDs {
		slog.Info("reviewer lease expired; the job was reset for redispatch", "workflow_run_id", workflowID)
	}
	return nil
}

// pgInterval converts a Go duration into the pgtype.Interval sqlc-generated
// queries expect for an `interval` column or cast, so every sweep timeout
// stays a single compiler-checked `time.Duration` constant instead of a
// separately hand-maintained SQL literal.
func pgInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func pgText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
}

// reclaimUnansweredOffers releases jobs a runner was offered and never answered.
// An offered job has no lease yet, so the lease sweep cannot see it, and its
// project lock is held from the moment the offer is written — a runner that
// takes an offer and dies before accepting would otherwise wedge that project
// with nothing to reclaim it.
func (s *Core) reclaimUnansweredOffers(ctx context.Context) error {
	if err := s.queries.ExpireUnansweredOffers(ctx, pgInterval(unansweredOffer)); err != nil {
		return databaseError(err)
	}
	workflowIDs, err := s.queries.CancelUnansweredOfferJobs(ctx, db.CancelUnansweredOfferJobsParams{
		Reason:          pgText(unansweredOfferReason),
		UnansweredOffer: pgInterval(unansweredOffer),
	})
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, func(ctx context.Context, workflowID string) error {
		// The issue is left eligible: nothing ran, so nothing was spent and
		// this work should simply be offered again.
		return s.terminateWorkflow(ctx, workflowID, StatusCancelled, "cancelled", unansweredOfferReason)
	})
}

// blockAbandonedChecks ends a wait for GitHub checks that is never going to
// resolve, so the project can schedule again.
func (s *Core) blockAbandonedChecks(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectAbandonedChecksWorkflows(ctx, pgInterval(abandonedChecks))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, func(ctx context.Context, workflowID string) error {
		return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "delivery.failed", "GitHub checks did not report a result within "+abandonedChecks.String())
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
func (s *Core) markStaleRunnersOffline(ctx context.Context) error {
	return databaseError(s.queries.MarkStaleRunnersOffline(ctx, pgInterval(staleRunner)))
}

// reclaimExpiredLeases fails jobs whose runner stopped renewing. The runner
// cannot rescue them itself: every write path it has is fenced on an unexpired
// lease, so once the lease lapses a reconnecting runner can neither renew it
// nor report the outcome, and the job would stay 'running' forever.
func (s *Core) reclaimExpiredLeases(ctx context.Context) error {
	workflowIDs, err := s.queries.CancelExpiredLeaseJobs(ctx, pgText(abandonedLease))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, func(ctx context.Context, workflowID string) error {
		return s.terminateWorkflow(ctx, workflowID, StatusFailed, "failed", abandonedLease)
	})
}

// resumeStrandedDeliveries re-drives workflows left at 'delivering'. A run
// reaches that status for the short window between the runner's completion
// being committed (persistExecutionEvent) and the pull request being opened
// (deliverWorkflow), and deliberately keeps its project lock across it. If the
// process dies in that window nothing else looks at the run again: the check
// observer only selects 'waiting_github_checks', and both retry and cancel
// refuse a run whose status is already terminal (delivering is not, but
// neither action is what re-drives a delivery).
//
// Before StatusDelivering existed this same window was spent at 'completed',
// indistinguishable by status alone from the run's other, later use of that
// value once GitHub confirmed the merge -- the only thing telling them apart
// was whether a project_locks row still happened to exist, which is why this
// query used to join app.project_locks. Splitting the two meanings gives the
// stranded case its own status, so a plain predicate on status and age finds
// exactly the same rows the lock join did.
//
// deliverWorkflow finds an existing pull request before creating one, so
// re-driving a delivery that already reached GitHub reuses it.
//
// The age bound is what keeps this from racing a delivery that is still in
// progress. A run matches this predicate for the whole time the inline delivery
// is talking to GitHub, and two concurrent deliveries would leave the loser's
// status update matching no row — reported as a delivery failure, which would
// block a run whose pull request had just been opened successfully.
func (s *Core) resumeStrandedDeliveries(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectStrandedDeliveryWorkflows(ctx, pgInterval(strandedDelivery))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, s.deliverWorkflow)
}

// resumeStrandedReviewDispatches re-drives a run stuck at 'waiting_ai_review'
// whose reviewer job was never actually offered -- the inline
// dispatchReviewerJob call persistExecutionEvent already made (or a previous
// sweep pass) found no connected runner, or lost a race to another attempt.
// See SelectStrandedReviewDispatchWorkflows (review.sql) for the exact guard.
func (s *Core) resumeStrandedReviewDispatches(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectStrandedReviewDispatchWorkflows(ctx, pgInterval(strandedReviewDispatch))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, s.dispatchReviewerJob)
}

// resumeStrandedReviewVerdicts re-drives a run stuck at 'waiting_ai_review'
// whose reviewer execution already finished (the job is 'completed' with role
// 'reviewer') but whose verdict was never applied -- the process died between
// persistExecutionEvent committing the terminal event and
// handleReviewCompletion's follow-on delivery/block decision. See
// SelectStrandedReviewVerdictWorkflows (review.sql) for the exact guard.
func (s *Core) resumeStrandedReviewVerdicts(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectStrandedReviewVerdictWorkflows(ctx, pgInterval(strandedReviewVerdict))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, s.applyRecordedReviewVerdict)
}

// resumeStrandedPipelineDecisions re-drives a run stuck at 'pipeline_failed'
// whose repair-or-block decision was never applied -- persistExecutionEvent
// committed the terminal event, then the process died, or
// dispatchPipelineRepairJob's own inline call found no connected or no
// eligible runner, before pipelineFailedOrBlock's decision landed. See
// SelectStrandedPipelineFailureWorkflows (repair.sql) for the exact guard.
func (s *Core) resumeStrandedPipelineDecisions(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectStrandedPipelineFailureWorkflows(ctx, pgInterval(strandedPipelineFailure))
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, s.applyStrandedPipelineDecision)
}

// applyStrandedPipelineDecision is resumeStrandedPipelineDecisions' per-workflow
// action: it re-reads the reason persistExecutionEvent already stored in
// blocking_reason when it set the run to StatusPipelineFailed (rather than
// re-parsing the original runner payload, not re-fetched here) and re-applies
// pipelineFailedOrBlock, the same decision the inline call already attempted.
func (s *Core) applyStrandedPipelineDecision(ctx context.Context, workflowID string) error {
	reason, err := s.queries.GetWorkflowBlockingReason(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	return s.pipelineFailedOrBlock(ctx, workflowID, reason)
}

// eachWorkflowID runs `do` against every already-fetched workflow identifier,
// collecting failures rather than stopping at the first. These batches are
// ordered oldest-first by their query, so returning early would let one
// persistently failing workflow sit at the head and starve the rest — and in
// the recovery sweep that head-of-line row is one holding a project lock, so a
// single bad row would block lock release for every other project.
func (s *Core) eachWorkflowID(ctx context.Context, workflowIDs []string, do func(context.Context, string) error) error {
	var failures []error
	for _, workflowID := range workflowIDs {
		if err := do(ctx, workflowID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
