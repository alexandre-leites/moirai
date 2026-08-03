-- name: UpsertPullRequest :exec
INSERT INTO app.pull_requests(id, workflow_run_id, provider, external_id, url, head_commit, state)
VALUES (sqlc.arg(id), sqlc.arg(workflow_run_id), sqlc.arg(provider), sqlc.arg(external_id), sqlc.arg(url), sqlc.arg(head_commit), sqlc.arg(state))
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

-- name: MarkWorkflowAwaitingApproval :execrows
-- observeWorkflow's guarded transition off 'waiting_github_checks' once GitHub
-- checks are green but the project opted into the human-approval gate. The
-- WHERE clause is what makes a second observer tick against the same run (it
-- is no longer selected by SelectWaitingGithubChecksWorkflows once this
-- lands) a no-op rather than a double transition.
UPDATE app.workflow_runs
SET status = 'waiting_human', current_phase = 'waiting_human', updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'waiting_github_checks';

-- name: MarkWorkflowApproved :execrows
-- SubmitHumanDecision's "approved" branch. Sends the run back to
-- 'waiting_github_checks' rather than merging directly here: the checks are
-- already green, so the next observer tick (or SubmitHumanDecision's own
-- best-effort immediate call into observeWorkflow) drives the merge through
-- the same tested path deliverWorkflow's non-gated runs already use, instead
-- of duplicating it. Guarded on 'waiting_human' so approving a run that is no
-- longer at the gate (already decided, retried, or cancelled out from under
-- the operator) reports "not awaiting approval" instead of silently doing
-- nothing or resurrecting a run that moved on.
UPDATE app.workflow_runs
SET status = 'waiting_github_checks', current_phase = 'waiting_github_checks', updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'waiting_human';

-- name: RejectWorkflowApproval :one
-- SubmitHumanDecision's "changes_requested" branch: the same terminal shape
-- as any other rejection (blockExternal, an agent's own declared block), but
-- guarded specifically on 'waiting_human' -- unlike TerminateWorkflowRun's
-- broader "not already terminal" guard -- so this action only ever fires
-- against a run genuinely sitting at the approval gate.
UPDATE app.workflow_runs
SET status = 'blocked', current_phase = 'blocked',
    blocking_reason = sqlc.arg(reason), terminal_reason = sqlc.arg(reason),
    completed_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'waiting_human'
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

-- name: ForceWorkflowCompleted :one
-- Used only when MarkWorkflowCompleted's guard above misses: GitHub has
-- already confirmed the pull request merged (observeWorkflow calls this only
-- after that), which is irreversible, regardless of what this run's status
-- column raced to in the meantime (an operator cancelling it, abandonedChecks
-- blocking it, or anything else). The WHERE clause deliberately does not
-- repeat the 'waiting_github_checks' guard: the merge is a fact this write
-- must land no matter which status it finds. previous_status lets the caller
-- log what the race actually was.
WITH previous AS (SELECT status FROM app.workflow_runs WHERE app.workflow_runs.id = sqlc.arg(id))
UPDATE app.workflow_runs
SET status = 'completed', current_phase = 'completed', completed_at = now(), updated_at = now(), delivery_attempts = 0
WHERE app.workflow_runs.id = sqlc.arg(id)
RETURNING (SELECT status FROM previous) AS previous_status;

-- name: MarkPullRequestMerged :exec
UPDATE app.pull_requests SET state = 'merged', merged_at = now() WHERE workflow_run_id = sqlc.arg(workflow_run_id);

-- name: InsertPullRequestMergedEvent :exec
INSERT INTO app.workflow_events(workflow_run_id, event_type, severity, payload)
VALUES (sqlc.arg(workflow_run_id), 'pull_request.merged', 'info', '{}'::jsonb);

-- name: GetDeliveryWorkflow :one
-- p.configuration travels along so observeWorkflow can decode
-- RequireHumanApproval without a second round trip: deliveryWorkflow() is
-- already the one place both deliverWorkflow and observeWorkflow fetch a
-- run's project-scoped delivery facts from.
SELECT wr.project_id::text AS project_id, wr.issue_id::text AS issue_id, i.external_id, i.title, i.body,
       COALESCE(p.repository_url, '') AS repository_url, p.default_branch, p.configuration,
       COALESCE(wr.branch_name, '') AS branch_name, pr.external_id AS pr_external_id, p.code_host_type
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
LEFT JOIN app.pull_requests pr ON pr.workflow_run_id = wr.id
WHERE wr.id = sqlc.arg(id);
