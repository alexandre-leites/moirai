-- name: ExpireUnansweredOffers :exec
UPDATE app.job_offers
SET status = 'expired', responded_at = now()
WHERE status = 'offered'
  AND job_id IN (
    SELECT id FROM app.jobs
    WHERE status = 'offered' AND offered_at < now() - sqlc.arg(unanswered_offer)::interval
  );

-- name: CancelUnansweredOfferJobs :many
UPDATE app.jobs
SET status = 'cancelled', finished_at = now(), lease_generation = lease_generation + 1,
    recovery_reason = sqlc.arg(reason)
WHERE status = 'offered' AND offered_at < now() - sqlc.arg(unanswered_offer)::interval
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
UPDATE app.jobs
SET status = 'cancelled', finished_at = now(), lease_generation = lease_generation + 1,
    recovery_reason = sqlc.arg(reason)
WHERE status IN ('preparing', 'running') AND lease_expires_at < now()
RETURNING workflow_run_id::text AS workflow_run_id;

-- name: SelectStrandedDeliveryWorkflows :many
SELECT wr.id::text AS id
FROM app.workflow_runs wr
JOIN app.project_locks l ON l.workflow_run_id = wr.id
WHERE wr.status = 'completed' AND wr.updated_at < now() - sqlc.arg(stranded_delivery)::interval
ORDER BY wr.updated_at, wr.id
LIMIT 20;
