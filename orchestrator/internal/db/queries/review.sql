-- name: GetProjectConfigForWorkflow :one
-- persistExecutionEvent reads this once per developer "completed" event to
-- decide whether the project opted into EnableAiReview (server.go's
-- projectConfig) -- a project that never touches this key behaves exactly as
-- it always has.
SELECT p.configuration
FROM app.workflow_runs wr
JOIN app.projects p ON p.id = wr.project_id
WHERE wr.id = sqlc.arg(id);

-- name: GetReviewDispatchWorkflow :one
-- Everything dispatchReviewerJob (review.go) needs to build and offer a
-- fresh, independent reviewer execution against the one job a workflow run
-- already has. Guarded on the run's own status and the job's own role and
-- status: only a run sitting at 'waiting_ai_review' whose job is still the
-- completed developer job is ever reviewable, so a concurrent or repeated
-- dispatch attempt (the recovery sweep racing the inline call
-- persistExecutionEvent already made) finds no row and is a no-op rather than
-- a double dispatch.
SELECT wr.project_id::text AS project_id, i.external_id, i.title, i.body,
       p.repository_mode, COALESCE(p.repository_url, '') AS repository_url,
       COALESCE(p.local_repository_path, '') AS local_repository_path,
       p.default_branch, COALESCE(wr.branch_name, '') AS branch_name,
       p.configuration, wr.review_cycles, j.id::text AS job_id
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
JOIN app.jobs j ON j.workflow_run_id = wr.id
WHERE wr.id = sqlc.arg(id) AND wr.status = 'waiting_ai_review' AND j.role = 'developer' AND j.status = 'completed';

-- name: SelectEligibleReviewRunner :one
-- Picks a runner to hand the reviewer packet to, restricted to the connected
-- set the caller already holds gRPC control streams for (sqlc.arg(runner_ids)).
-- Deliberately simpler than ClaimSchedulableIssue: there is no competing
-- issue to arbitrate over here, only one workflow's one review, so this reads
-- without FOR UPDATE/SKIP LOCKED -- a race loses at ReopenJobForReview's own
-- guard instead, which is a cheap, harmless no-op.
SELECT r.id::text AS runner_id
FROM app.runners r
WHERE r.status = 'online' AND r.enabled AND NOT r.draining AND r.revoked_at IS NULL
  AND r.id::text = ANY(sqlc.arg(runner_ids)::text[])
  AND r.labels @> sqlc.arg(required_labels)::jsonb
  AND (SELECT COUNT(*) FROM app.jobs j WHERE j.runner_id = r.id AND j.status IN ('offered', 'preparing', 'running')) < r.capacity
ORDER BY r.id
LIMIT 1;

-- name: ReopenJobForReview :one
-- Reuses the workflow run's single job row for a second, independent
-- execution instead of inserting a new one (app.jobs.workflow_run_id stays
-- UNIQUE). Guarded on the job still being the completed developer job, the
-- same guard GetReviewDispatchWorkflow reads under -- a second caller that
-- raced this one finds 0 rows affected and does nothing further.
UPDATE app.jobs
SET role = 'reviewer', runner_id = sqlc.arg(runner_id)::uuid, status = 'offered',
    offered_at = now(), accepted_at = NULL, started_at = NULL, finished_at = NULL,
    last_event_sequence = 0, lease_generation = lease_generation + 1, recovery_reason = NULL
WHERE id = sqlc.arg(id) AND role = 'developer' AND status = 'completed'
RETURNING lease_generation;

-- name: IncrementReviewCycles :exec
UPDATE app.workflow_runs SET review_cycles = review_cycles + 1, updated_at = now() WHERE id = sqlc.arg(id);

-- name: InsertAiReview :exec
INSERT INTO app.ai_reviews(id, workflow_run_id, commit_sha, verdict, result)
VALUES (sqlc.arg(id), sqlc.arg(workflow_run_id), sqlc.arg(commit_sha), sqlc.arg(verdict), sqlc.arg(result)::jsonb);

-- name: MarkWorkflowReviewApproved :execrows
-- An approving verdict's handoff to deliverWorkflow, which itself requires
-- 'delivering' (MarkWorkflowDelivered's own guard) -- the same status a
-- developer's own "completed" event moves a run through when AI review is
-- disabled. Guarded on 'waiting_ai_review' so this is a no-op rather than a
-- double transition if it ever raced another caller.
UPDATE app.workflow_runs
SET status = 'delivering', current_phase = 'delivering', updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'waiting_ai_review';

-- name: GetLatestAiReview :one
SELECT verdict FROM app.ai_reviews WHERE workflow_run_id = sqlc.arg(workflow_run_id) ORDER BY created_at DESC LIMIT 1;

-- name: SelectStrandedReviewDispatchWorkflows :many
-- resumeStrandedReviewDispatches' (recovery.go) candidate set: a run whose
-- developer execution completed and whose project opted into AI review, but
-- whose inline dispatchReviewerJob call never actually offered a reviewer job
-- -- typically because no runner was connected yet. The same guard
-- GetReviewDispatchWorkflow uses (job still the completed developer job)
-- doubles as "dispatch has not happened yet", so a run this sweep already
-- redispatched successfully stops matching on its own.
SELECT wr.id::text AS id
FROM app.workflow_runs wr
JOIN app.jobs j ON j.workflow_run_id = wr.id
WHERE wr.status = 'waiting_ai_review' AND j.role = 'developer' AND j.status = 'completed'
  AND wr.updated_at < now() - sqlc.arg(stranded_review)::interval
ORDER BY wr.updated_at, wr.id
LIMIT 20;

-- name: SelectStrandedReviewVerdictWorkflows :many
-- resumeStrandedReviewVerdicts' (recovery.go) candidate set: a reviewer
-- execution finished (the job is 'completed' with role 'reviewer'), but the
-- run is still sitting at 'waiting_ai_review' -- persistExecutionEvent
-- committed the terminal event, then the process died before
-- handleReviewCompletion's follow-on delivery/block decision landed.
SELECT wr.id::text AS id
FROM app.workflow_runs wr
JOIN app.jobs j ON j.workflow_run_id = wr.id
WHERE wr.status = 'waiting_ai_review' AND j.role = 'reviewer' AND j.status = 'completed'
  AND wr.updated_at < now() - sqlc.arg(stranded_review)::interval
ORDER BY wr.updated_at, wr.id
LIMIT 20;
