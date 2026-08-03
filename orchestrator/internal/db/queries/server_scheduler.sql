-- name: ListQueueEntries :many
SELECT p.id::text AS project_id, p.name AS project_name, i.external_id, i.title, i.priority,
       CASE WHEN NOT p.enabled THEN 'project_disabled'
            WHEN EXISTS (SELECT 1 FROM app.project_locks l WHERE l.project_id = p.id) THEN 'project_locked'
            ELSE '' END AS blocked_reason
FROM app.issues i
JOIN app.projects p ON p.id = i.project_id
WHERE i.eligible AND i.state = 'open' AND NOT EXISTS (
  SELECT 1 FROM app.workflow_runs w
  WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed', 'blocked', 'completed')
)
ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at, i.project_id, i.external_id
LIMIT $1;

-- name: GetSchedulerSnapshot :one
-- The oldest heartbeat is returned as a raw timestamp (rather than an
-- already-subtracted epoch age) plus the database's own now(), so the
-- nullability of "no enabled runner" survives sqlc's type inference as a
-- pgtype.Timestamptz.Valid flag instead of silently becoming a NOT NULL
-- float64 that panics on a NULL scan -- see the Go call site for the
-- subtraction, which still uses only values this same statement read from
-- the database's clock.
SELECT
  (SELECT COUNT(*) FROM app.issues i JOIN app.projects p ON p.id = i.project_id
   WHERE p.enabled AND i.eligible AND i.state = 'open' AND NOT EXISTS (
     SELECT 1 FROM app.workflow_runs w
     WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed', 'blocked', 'completed')
   )) AS queue_depth,
  (SELECT COUNT(*) FROM app.workflow_runs WHERE status NOT IN ('completed','failed','blocked','cancelled')) AS active_workflows,
  (SELECT COUNT(*) FROM app.jobs WHERE status IN ('offered','preparing','running')) AS scheduled_jobs,
  (SELECT COUNT(*) FROM app.runners WHERE enabled AND revoked_at IS NULL) AS enabled_runners,
  hb.oldest_heartbeat::timestamptz AS oldest_heartbeat,
  now()::timestamptz AS db_now
FROM (SELECT 1) AS one
LEFT JOIN LATERAL (
  SELECT MIN(COALESCE(last_seen_at, registered_at)) AS oldest_heartbeat
  FROM app.runners
  WHERE enabled AND revoked_at IS NULL
  HAVING COUNT(*) > 0
) hb ON true;

-- name: ClaimSchedulableIssue :one
-- A runner's in-flight job count is compared against its own registered
-- capacity (app.runners.capacity, see migration 003) rather than excluding it
-- the instant it holds any job at all: that used to hard-cap every runner at
-- one concurrent execution regardless of what it advertised at registration.
-- The count is still scoped to this runner (j.runner_id = r.id) so it says
-- nothing about any other runner's load, and FOR UPDATE OF i, r still locks
-- exactly the issue and runner rows a concurrent ClaimSchedulableIssue would
-- also need, so SKIP LOCKED continues to hand a racing caller a different
-- issue/runner pair instead of double-booking this one.
SELECT i.id::text AS issue_id, i.external_id, i.title, i.body,
       p.id::text AS project_id, p.repository_mode, p.repository_url, p.local_repository_path, p.default_branch, p.configuration::text AS configuration,
       r.id::text AS runner_id
FROM app.issues i
JOIN app.projects p ON p.id = i.project_id
JOIN app.runners r ON r.status = 'online' AND r.enabled AND NOT r.draining AND r.revoked_at IS NULL
WHERE i.eligible AND i.state = 'open' AND p.enabled
  AND r.id::text = ANY(sqlc.arg(runner_ids)::text[])
  AND r.labels @> COALESCE(p.configuration->'required_runner_labels', '[]'::jsonb)
  AND NOT EXISTS (SELECT 1 FROM app.project_locks l WHERE l.project_id = p.id)
  AND (SELECT COUNT(*) FROM app.jobs j WHERE j.runner_id = r.id AND j.status IN ('offered','preparing','running')) < r.capacity
  AND NOT EXISTS (
    SELECT 1 FROM app.workflow_runs w
    WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed', 'blocked', 'completed')
  )
ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at, i.project_id, i.external_id, r.id
FOR UPDATE OF i, r SKIP LOCKED
LIMIT 1;

-- name: CreateWorkflowRun :exec
INSERT INTO app.workflow_runs(id, project_id, issue_id, thread_id, status, current_phase, branch_name)
VALUES ($1, $2, $3, $4, 'offered', 'offered', $5);

-- name: CreateProjectLock :execrows
INSERT INTO app.project_locks(project_id, workflow_run_id)
VALUES ($1, $2)
ON CONFLICT(project_id) DO NOTHING;

-- name: CreateJob :exec
-- role (028_workflow_run_ai_review.sql, #353) is 'developer' for every
-- ordinary dispatch, matching the column's own default -- ScheduleOnce passes
-- 'planner' instead for a project that opted into RequirePlanning (#351), so
-- the run's first execution is a planning one rather than the developer one.
INSERT INTO app.jobs(id, workflow_run_id, project_id, runner_id, status, role, lease_generation, offered_at)
VALUES (sqlc.arg(id), sqlc.arg(workflow_run_id), sqlc.arg(project_id), sqlc.arg(runner_id)::uuid, 'offered', sqlc.arg(role), 1, now());

-- name: CreateJobOffer :exec
INSERT INTO app.job_offers(id, job_id, runner_id, status, expires_at)
VALUES ($1, $2, $3, 'offered', 'infinity');

-- name: CancelOfferedJob :exec
UPDATE app.jobs SET status = 'cancelled', finished_at = now(), recovery_reason = $2
WHERE id = $1 AND status = 'offered';

-- name: CancelJobOfferByJob :exec
UPDATE app.job_offers SET status = 'cancelled', responded_at = now()
WHERE job_id = $1 AND status = 'offered';

-- name: CancelWorkflowRunUndelivered :exec
UPDATE app.workflow_runs SET status = 'cancelled', current_phase = 'cancelled', completed_at = now(), terminal_reason = $2
WHERE id = $1;

-- DeleteProjectLock lives in delivery.sql (identical statement, already
-- generated there); reused as-is rather than duplicated.

-- name: AcceptJobOffer :one
UPDATE app.job_offers SET status = 'accepted', responded_at = now()
WHERE job_id = $1 AND runner_id = $2 AND status = 'offered' AND expires_at > now()
RETURNING id::text AS id;

-- name: AcceptJob :one
-- role travels back to acceptOffer (server.go) so it can decide whether to
-- call SetWorkflowPreparing at all: a reviewer's job offer being accepted
-- must not move the run off StatusWaitingAiReview the way a developer's
-- always has (see acceptOffer's own comment for why).
UPDATE app.jobs SET status = 'preparing', accepted_at = now(), lease_expires_at = now() + sqlc.arg(lease_duration)::interval
WHERE id = sqlc.arg(id) AND runner_id = sqlc.arg(runner_id)::uuid AND status = 'offered'
RETURNING lease_generation, lease_expires_at, role;

-- name: SetWorkflowPreparing :exec
UPDATE app.workflow_runs SET status = 'preparing', updated_at = now()
WHERE app.workflow_runs.id = (SELECT j.workflow_run_id FROM app.jobs j WHERE j.id = $1)
  AND status NOT IN ('completed','failed','blocked','cancelled');

-- name: SetWorkflowPlanningActive :exec
-- acceptOffer's planner-role branch: the same shape SetWorkflowPreparing is,
-- called instead of it when the accepted job's role (AcceptJob's own RETURNING)
-- is 'planner' rather than 'developer'.
UPDATE app.workflow_runs SET status = 'planning', current_phase = 'planning', updated_at = now()
WHERE app.workflow_runs.id = (SELECT j.workflow_run_id FROM app.jobs j WHERE j.id = $1)
  AND status NOT IN ('completed','failed','blocked','cancelled');

-- name: IncrementPlanningAttempts :exec
UPDATE app.workflow_runs SET planning_attempts = planning_attempts + 1, updated_at = now() WHERE id = $1;

-- name: RecordPlanSummary :exec
UPDATE app.workflow_runs SET plan_summary = NULLIF(sqlc.arg(plan_summary)::text, ''), updated_at = now() WHERE id = sqlc.arg(id);

-- name: ReopenJobForImplementation :one
-- Reuses the workflow run's single job row for its developer execution once
-- planning has completed (app.jobs.workflow_run_id stays UNIQUE), exactly the
-- shape ReopenJobForReview (review.sql, #353) reuses it for an independent
-- review after a developer execution -- here reopened backwards, from
-- 'planner' to 'developer', as the very first execution's follow-up rather
-- than a second one after it. Guarded on the job still being the completed
-- planner job, the same guard a racing caller (there is only ever one, this
-- is not retried by any sweep) would lose.
--
-- The runner's own offer/lease state (runner/internal/control's OfferState)
-- keys everything by job ID and forgets it entirely once Abandon runs at the
-- end of the planner execution, so a fresh JobOffer for the same ID with an
-- incremented lease_generation is indistinguishable, to the runner, from any
-- other new job -- no runner-side change was needed for this reopening.
UPDATE app.jobs SET
  role = 'developer', status = 'offered', lease_generation = lease_generation + 1, last_event_sequence = 0,
  offered_at = now(), accepted_at = NULL, started_at = NULL, finished_at = NULL, recovery_reason = NULL
WHERE id = $1 AND role = 'planner' AND status = 'completed'
RETURNING lease_generation, runner_id::text AS runner_id;

-- name: GetImplementationDispatchFacts :one
-- Everything dispatchImplementationJob needs to build a developer packet for
-- a workflow whose planning execution just completed, mirroring what
-- ClaimSchedulableIssue hands ScheduleOnce for the ordinary (no-planning)
-- dispatch.
SELECT wr.project_id::text AS project_id, wr.branch_name,
       i.external_id, i.title, i.body,
       p.repository_mode, p.repository_url, p.local_repository_path, p.default_branch,
       p.configuration::text AS configuration
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
WHERE wr.id = $1;

-- name: GetJobForOfferReject :one
SELECT j.workflow_run_id::text AS workflow_run_id, j.project_id::text AS project_id
FROM app.jobs j
JOIN app.job_offers o ON o.job_id = j.id
WHERE j.id = $1 AND o.runner_id = $2 AND o.status = 'offered' AND o.expires_at > now()
FOR UPDATE;

-- name: CancelJobOfferByRunner :exec
UPDATE app.job_offers SET status = 'rejected', responded_at = now()
WHERE job_id = $1 AND runner_id = $2;

-- name: CancelJob :exec
UPDATE app.jobs SET status = 'cancelled', finished_at = now(), recovery_reason = NULLIF($2, '')
WHERE id = $1;

-- name: CancelWorkflowRunOfferRejected :exec
UPDATE app.workflow_runs SET status = 'cancelled', current_phase = 'cancelled', terminal_reason = 'runner rejected offer', completed_at = now(), updated_at = now()
WHERE id = $1;

-- name: RenewJobLease :one
UPDATE app.jobs SET lease_expires_at = sqlc.arg(lease_expires_at)
WHERE id = sqlc.arg(id) AND runner_id = sqlc.arg(runner_id)::uuid AND lease_generation = sqlc.arg(lease_generation) AND status IN ('preparing','running') AND lease_expires_at > now()
RETURNING lease_expires_at;

-- name: RecordJobExecutionEvent :one
UPDATE app.jobs SET
  last_event_sequence = sqlc.arg(event_sequence),
  status = CASE WHEN sqlc.arg(event_type) = 'started' THEN 'running' WHEN sqlc.arg(event_type) IN ('completed','failed','cancelled') THEN sqlc.arg(event_type) ELSE status END,
  started_at = CASE WHEN sqlc.arg(event_type) = 'started' THEN COALESCE(started_at, now()) ELSE started_at END,
  finished_at = CASE WHEN sqlc.arg(event_type) IN ('completed','failed','cancelled') THEN now() ELSE finished_at END
WHERE id = sqlc.arg(id) AND runner_id = sqlc.arg(runner_id)::uuid AND lease_generation = sqlc.arg(lease_generation) AND status IN ('preparing','running') AND lease_expires_at > now() AND last_event_sequence < sqlc.arg(event_sequence)
RETURNING workflow_run_id::text AS workflow_run_id, role;

-- name: CreateWorkflowEvent :exec
INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload)
VALUES ($1, $2, $3, $4::jsonb);

-- name: SetWorkflowTerminalStatus :exec
UPDATE app.workflow_runs SET
  status = $2,
  current_phase = $2,
  blocking_reason = COALESCE(NULLIF($3, ''), blocking_reason),
  terminal_reason = COALESCE(NULLIF($3, ''), terminal_reason),
  updated_at = now(),
  completed_at = CASE WHEN $2 IN ('completed','failed','blocked','cancelled') THEN now() ELSE completed_at END
WHERE id = $1;

-- name: DeleteProjectLockByWorkflow :exec
DELETE FROM app.project_locks WHERE workflow_run_id = $1;
