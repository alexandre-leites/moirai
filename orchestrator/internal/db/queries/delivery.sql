-- name: UpsertPullRequest :exec
INSERT INTO app.pull_requests(id, workflow_run_id, provider, external_id, url, head_commit, state)
VALUES (sqlc.arg(id), sqlc.arg(workflow_run_id), 'github', sqlc.arg(external_id), sqlc.arg(url), sqlc.arg(head_commit), sqlc.arg(state))
ON CONFLICT (workflow_run_id) DO UPDATE SET
  external_id = EXCLUDED.external_id,
  url = EXCLUDED.url,
  head_commit = EXCLUDED.head_commit,
  state = EXCLUDED.state;

-- name: MarkWorkflowDelivered :execrows
UPDATE app.workflow_runs
SET status = 'waiting_github_checks', current_phase = 'waiting_github_checks', updated_at = now(), completed_at = NULL, delivery_attempts = 0
WHERE id = sqlc.arg(id) AND status = 'delivering';

-- name: RecordTransientDeliveryFailure :one
-- Bumps the count of consecutive transient GitHub failures blockOrRetryExternal
-- (delivery.go) has absorbed for this run without moving its status, and
-- returns the new count so the caller can decide whether to keep retrying or
-- fall through to blockExternal. updated_at advances with it so a transient
-- failure during 'delivering' also pushes back resumeStrandedDeliveries'
-- re-drive window (recovery.go's strandedDelivery), which is what turns
-- these retries into spaced-out attempts instead of a tight loop.
UPDATE app.workflow_runs
SET delivery_attempts = delivery_attempts + 1, updated_at = now()
WHERE id = sqlc.arg(id)
RETURNING delivery_attempts;

-- name: InsertPullRequestCreatedEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES (sqlc.arg(workflow_run_id), 'pull_request.created', 'info', sqlc.arg(payload)::jsonb);

-- name: SelectWaitingGithubChecksWorkflows :many
SELECT wr.id::text AS id
FROM app.workflow_runs wr
WHERE wr.status = 'waiting_github_checks'
ORDER BY wr.updated_at, wr.id
LIMIT 20;

-- name: TerminateWorkflowRun :one
UPDATE app.workflow_runs
SET status = sqlc.arg(status), current_phase = sqlc.arg(status),
    blocking_reason = sqlc.arg(reason), terminal_reason = sqlc.arg(reason),
    completed_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND status NOT IN ('failed', 'blocked', 'cancelled')
RETURNING project_id::text AS project_id;

-- name: DeleteProjectLock :exec
DELETE FROM app.project_locks WHERE project_id = sqlc.arg(project_id) AND workflow_run_id = sqlc.arg(workflow_run_id);

-- name: InsertWorkflowTerminationEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES (sqlc.arg(workflow_run_id), sqlc.arg(event_type), 'error', sqlc.arg(payload)::jsonb);

-- name: MarkWorkflowCompleted :execrows
UPDATE app.workflow_runs
SET status = 'completed', current_phase = 'completed', completed_at = now(), updated_at = now(), delivery_attempts = 0
WHERE id = sqlc.arg(id) AND status = 'waiting_github_checks';

-- name: MarkPullRequestMerged :exec
UPDATE app.pull_requests SET state = 'merged', merged_at = now() WHERE workflow_run_id = sqlc.arg(workflow_run_id);

-- name: InsertPullRequestMergedEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES (sqlc.arg(workflow_run_id), 'pull_request.merged', 'info', '{}'::jsonb);

-- name: GetDeliveryWorkflow :one
SELECT wr.project_id::text AS project_id, wr.issue_id::text AS issue_id, i.external_id, i.title, i.body,
       COALESCE(p.repository_url, '') AS repository_url, p.default_branch,
       COALESCE(wr.branch_name, '') AS branch_name, pr.external_id AS pr_external_id
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
LEFT JOIN app.pull_requests pr ON pr.workflow_run_id = wr.id
WHERE wr.id = sqlc.arg(id);
