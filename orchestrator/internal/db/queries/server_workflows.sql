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
       wr.review_cycles, wr.total_agent_executions, wr.created_at, wr.updated_at
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
       wr.review_cycles, wr.total_agent_executions, wr.created_at, wr.updated_at
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
LEFT JOIN app.pull_requests pr ON pr.workflow_run_id = wr.id
WHERE wr.id = $1;

-- name: GetWorkflowForControl :one
SELECT project_id::text AS project_id, status
FROM app.workflow_runs
WHERE id = $1
FOR UPDATE;

-- name: ReopenIssueForRetry :exec
UPDATE app.issues SET eligible = true
WHERE app.issues.id = (SELECT wr.issue_id FROM app.workflow_runs wr WHERE wr.id = $1) AND state = 'open';

-- name: CreateRetryEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES ($1, 'workflow_transition', 'info', '{"reason":"reopened by manual retry"}'::jsonb);

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
