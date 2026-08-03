-- name: ExpireUnansweredOffers :exec
UPDATE app.job_offers
SET status = 'expired', responded_at = now()
WHERE status = 'offered'
  AND job_id IN (
    SELECT id FROM app.jobs
    WHERE status = 'offered' AND offered_at < now() - sqlc.arg(unanswered_offer)::interval
  );

-- name: CancelUnansweredOfferJobs :many
-- Scoped to role IN ('developer', 'planner'): an unanswered reviewer offer
-- must not cancel the whole workflow run the way an unanswered developer or
-- planner offer does (nothing ran yet for either of those, so the issue is
-- simply offered again) -- the developer's work already happened by the time
-- a reviewer offer exists, and would otherwise be discarded. See
-- resumeStrandedReviewDispatches (recovery.go), which redrives that case
-- instead by age alone. A planner offer belongs with the developer one here,
-- not with the reviewer: it is the first execution a run makes, exactly like
-- an ordinary developer offer, just under a different role.
UPDATE app.jobs
SET status = 'cancelled', finished_at = now(), lease_generation = lease_generation + 1,
    recovery_reason = sqlc.arg(reason)
WHERE status = 'offered' AND role IN ('developer', 'planner') AND offered_at < now() - sqlc.arg(unanswered_offer)::interval
RETURNING workflow_run_id::text AS workflow_run_id;

-- name: SelectAbandonedChecksWorkflows :many
SELECT id::text AS id
FROM app.workflow_runs
WHERE status = 'waiting_github_checks' AND updated_at < now() - sqlc.arg(abandoned_checks)::interval
ORDER BY updated_at, id
LIMIT 20;

-- name: MarkStaleRunnersOffline :exec
UPDATE app.runners
SET status = 'offline'
WHERE status = 'online'
  AND (last_seen_at IS NULL OR last_seen_at < now() - sqlc.arg(stale_runner)::interval);

-- name: CancelExpiredLeaseJobs :many
-- Scoped to role IN ('developer', 'planner') for the same reason
-- CancelUnansweredOfferJobs is: failing the whole workflow run over a
-- reviewer's lapsed lease would discard a developer execution that already
-- succeeded, but a planner's lapsed lease has nothing of the sort behind it
-- yet -- it belongs with the developer case, failing the run outright, not
-- with the reviewer's. See ReclaimExpiredReviewLeases for the reviewer-scoped
-- counterpart.
UPDATE app.jobs
SET status = 'cancelled', finished_at = now(), lease_generation = lease_generation + 1,
    recovery_reason = sqlc.arg(reason)
WHERE status IN ('preparing', 'running') AND role IN ('developer', 'planner') AND lease_expires_at < now()
RETURNING workflow_run_id::text AS workflow_run_id;

-- name: ReclaimUnansweredReviewOffers :many
-- The reviewer-scoped counterpart of CancelUnansweredOfferJobs: instead of
-- cancelling the run, resets its job back to the shape
-- GetReviewDispatchWorkflow/SelectStrandedReviewDispatchWorkflows expect (a
-- completed developer job), so the next dispatch attempt -- the recovery
-- sweep's resumeStrandedReviewDispatches, on its next tick -- redrives it
-- against a (possibly different) connected runner.
UPDATE app.jobs
SET status = 'completed', role = 'developer', recovery_reason = sqlc.arg(reason)
WHERE status = 'offered' AND role = 'reviewer' AND offered_at < now() - sqlc.arg(unanswered_offer)::interval
RETURNING workflow_run_id::text AS workflow_run_id;

-- name: ReclaimExpiredReviewLeases :many
-- The reviewer-scoped counterpart of CancelExpiredLeaseJobs: a runner that
-- stopped renewing a reviewer lease loses that attempt, not the run -- see
-- ReclaimUnansweredReviewOffers for why this resets rather than cancels.
UPDATE app.jobs
SET status = 'completed', role = 'developer', recovery_reason = sqlc.arg(reason)
WHERE status IN ('preparing', 'running') AND role = 'reviewer' AND lease_expires_at < now()
RETURNING workflow_run_id::text AS workflow_run_id;

-- name: SelectStrandedDeliveryWorkflows :many
SELECT wr.id::text AS id
FROM app.workflow_runs wr
WHERE wr.status = 'delivering' AND wr.updated_at < now() - sqlc.arg(stranded_delivery)::interval
ORDER BY wr.updated_at, wr.id
LIMIT 20;
