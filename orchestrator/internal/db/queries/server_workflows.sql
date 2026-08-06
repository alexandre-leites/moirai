-- name: ListWorkflowsPage :many
-- Replaces the old ListWorkflowIDs + one GetWorkflowDetail per row: that shape
-- was O(all workflow runs) in both rows and queries, and app.workflow_runs
-- only grows. This does the same join GetWorkflowDetail does, once, for the
-- most recent $1 rows -- see the pr.external_id/wr.pull_request_external_id
-- comment on GetWorkflowDetail below for why those aren't COALESCEd in SQL.
SELECT wr.id::text AS id, wr.project_id::text AS project_id, wr.status, wr.current_phase,
       i.external_id, i.title, wr.branch_name,
       pr.external_id AS pr_external_id, wr.pull_request_external_id AS run_pull_request_external_id,
       pr.url AS pr_url, wr.pull_request_url AS run_pull_request_url,
       pr.state AS pull_request_state, wr.blocking_reason,
       wr.planning_attempts, wr.implementation_attempts, wr.pipeline_repair_attempts, wr.ci_repair_attempts,
       wr.review_cycles, wr.total_agent_executions, wr.plan_summary, wr.created_at, wr.updated_at
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
LEFT JOIN app.pull_requests pr ON pr.workflow_run_id = wr.id
ORDER BY wr.created_at DESC, wr.id
LIMIT $1;

-- name: ListWorkflowEvents :many
SELECT id::text AS id, event_type, created_at, payload::text AS payload
FROM app.workflow_events
WHERE workflow_run_id = $1 AND id > $2
ORDER BY id
LIMIT $3;

-- name: GetWorkflowDetail :one
-- pr.external_id/pr.url and wr.pull_request_external_id/wr.pull_request_url are
-- combined in Go rather than with SQL COALESCE: sqlc's nullability inference
-- does not reliably propagate through COALESCE of two nullable columns (one
-- from an outer join), and mistyping either side NOT NULL would panic the
-- very first workflow scanned before a pull request exists.
SELECT wr.id::text AS id, wr.project_id::text AS project_id, wr.status, wr.current_phase,
       i.external_id, i.title, wr.branch_name,
       pr.external_id AS pr_external_id, wr.pull_request_external_id AS run_pull_request_external_id,
       pr.url AS pr_url, wr.pull_request_url AS run_pull_request_url,
       pr.state AS pull_request_state, wr.blocking_reason,
       wr.planning_attempts, wr.implementation_attempts, wr.pipeline_repair_attempts, wr.ci_repair_attempts,
       wr.review_cycles, wr.total_agent_executions, wr.plan_summary, wr.created_at, wr.updated_at
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
LEFT JOIN app.pull_requests pr ON pr.workflow_run_id = wr.id
WHERE wr.id = $1;

-- name: GetWorkflowForControl :one
SELECT project_id::text AS project_id, status
FROM app.workflow_runs
WHERE id = $1
FOR UPDATE;

-- name: SupersedeWorkflowRun :exec
-- Retry no longer writes app.issues.eligible directly (see #268 and migration
-- 023): the issue's eligible bit stays purely label-driven, and marking this
-- run superseded is what lets the scheduler's app.workflow_runs join stop
-- excluding its issue on this run's account. A fresh run is what actually
-- reopens the work; this only clears the way for one.
UPDATE app.workflow_runs SET superseded_at = now() WHERE id = $1 AND superseded_at IS NULL;

-- name: CreateRetryEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES ($1, 'workflow_transition', 'info', '{"reason":"reopened by manual retry"}'::jsonb);

-- name: GetRetryControlJob :one
-- Retry-with-context's (controlWorkflow, server.go) read of the job a
-- terminal run already has, so the re-arm keeps its current role -- the step
-- the run died at -- rather than always restarting the developer step from
-- scratch. app.jobs.workflow_run_id is UNIQUE, so one row.
SELECT j.id::text AS job_id, j.role, j.status AS job_status
FROM app.jobs j
WHERE j.workflow_run_id = $1
FOR UPDATE;

-- name: ResetWorkflowRunForRetry :execrows
-- Re-arms a terminal run for retry-with-context: back to the in-flight status
-- its job's role implies ('planning' for a planner, 'preparing' for a
-- developer -- the role is read in Go before this runs), terminal state
-- cleared, total_agent_executions bumped so each retry execution gets a
-- distinct execution ID. The project lock is deliberately left in place: the
-- run is still doing the same work, and a competing fresh run must not start.
UPDATE app.workflow_runs SET
  status = $2, current_phase = $2,
  terminal_reason = NULL, completed_at = NULL,
  total_agent_executions = total_agent_executions + 1,
  updated_at = now()
WHERE id = $1 AND status IN ('failed','blocked','cancelled') AND superseded_at IS NULL;

-- name: ReopenJobForRetry :one
-- Resets a terminal run's one job back to 'offered' for retry-with-context,
-- keeping its current role (the step that failed) and bumping the lease
-- generation so any stale runner event from the previous attempt is fenced.
-- Guarded on the job still being the terminal one whose failure the retry is
-- resuming; a second caller that raced this one finds 0 rows and does nothing.
UPDATE app.jobs SET
  status = 'offered', offered_at = now(), accepted_at = NULL, started_at = NULL,
  finished_at = NULL, last_event_sequence = 0, lease_generation = lease_generation + 1,
  recovery_reason = NULL
WHERE workflow_run_id = $1 AND status IN ('failed','cancelled')
RETURNING lease_generation, role;

-- name: GetRetryDispatchFacts :one
-- Everything dispatchRetryJob (server.go) needs to re-offer a retried run's
-- job with the accumulated context carried forward: the planner's plan (the
-- most recent plan.recorded event's payload) and the previous execution's own
-- account of its failure (summary + remainingWork of the last failed/cancelled
-- event). Guarded on the job still sitting at 'offered' and the run at the
-- in-flight status the re-arm set -- a second dispatch attempt finds no row
-- and does nothing.
SELECT wr.project_id::text AS project_id, i.external_id, i.title, i.body,
       p.repository_mode, COALESCE(p.repository_url, '') AS repository_url,
       COALESCE(p.local_repository_path, '') AS local_repository_path,
       p.default_branch, COALESCE(wr.branch_name, '') AS branch_name,
       p.configuration, j.id::text AS job_id, j.role, j.lease_generation,
       COALESCE(wr.plan_summary, '') AS plan_summary,
       COALESCE(plan_e.payload::text, '{}')::text AS plan_payload,
       COALESCE(fail_e.payload::text, '{}')::text AS failure_payload
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
JOIN app.jobs j ON j.workflow_run_id = wr.id
LEFT JOIN LATERAL (
  SELECT payload FROM app.workflow_events
  WHERE workflow_run_id = wr.id AND event_type = 'plan.recorded'
  ORDER BY id DESC LIMIT 1
) plan_e ON true
LEFT JOIN LATERAL (
  SELECT payload FROM app.workflow_events
  WHERE workflow_run_id = wr.id AND event_type IN ('failed','cancelled')
  ORDER BY id DESC LIMIT 1
) fail_e ON true
WHERE wr.id = sqlc.arg(id) AND wr.status IN ('preparing','planning') AND j.status = 'offered' AND wr.superseded_at IS NULL;

-- name: SetWorkflowControlStatus :exec
UPDATE app.workflow_runs SET
  status = $2,
  current_phase = $2,
  blocking_reason = CASE WHEN $2 = 'blocked' THEN $3 ELSE blocking_reason END,
  terminal_reason = $3,
  completed_at = now(),
  updated_at = now()
WHERE id = $1;

-- name: CancelWorkflowJobs :one
UPDATE app.jobs SET status = 'cancelled', finished_at = now(), lease_generation = lease_generation + 1, recovery_reason = $2
WHERE workflow_run_id = $1 AND status IN ('offered','preparing','running')
RETURNING id::text AS id, runner_id::text AS runner_id, lease_generation - 1 AS previous_lease_generation;

-- name: CancelWorkflowJobOffers :exec
UPDATE app.job_offers SET status = 'cancelled', responded_at = now()
WHERE status = 'offered' AND job_id IN (SELECT id FROM app.jobs WHERE workflow_run_id = $1);

-- name: CancelWorkflowExecutionRequests :exec
UPDATE app.workflow_execution_requests SET status = 'cancelled'
WHERE workflow_run_id = $1 AND status IN ('queued','dispatched');
