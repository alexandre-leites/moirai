-- name: ListProjectIDs :many
SELECT id::text AS id FROM app.projects ORDER BY name, id;

-- name: CreateProject :exec
INSERT INTO app.projects (id, name, repository_mode, repository_url, local_repository_path, default_branch, configuration)
VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7::jsonb);

-- name: UpdateProject :execrows
UPDATE app.projects
SET name = $2, repository_mode = $3, repository_url = NULLIF($4, ''), local_repository_path = NULLIF($5, ''),
    default_branch = $6, configuration = $7::jsonb, updated_at = now()
WHERE id = $1;

-- name: SetProjectEnabled :execrows
UPDATE app.projects SET enabled = $2, updated_at = now() WHERE id = $1;

-- name: GetProject :one
SELECT id::text AS id, name, enabled, repository_mode, repository_url, local_repository_path, default_branch, configuration::text AS configuration
FROM app.projects
WHERE id = $1;

-- name: ListProjectPipelineSteps :many
SELECT command, timeout_seconds, position, required
FROM app.project_pipeline_steps
WHERE project_id = $1
ORDER BY position, id;

-- name: DeleteProjectPipelineSteps :exec
DELETE FROM app.project_pipeline_steps WHERE project_id = $1;

-- name: CreateProjectPipelineStep :exec
INSERT INTO app.project_pipeline_steps(id, project_id, position, name, command, timeout_seconds, required)
VALUES ($1, $2, $3, $4, $4, $5, $6);

-- name: ListSyncableProjects :many
SELECT id::text AS id, repository_url
FROM app.projects
WHERE enabled;

-- name: ListSyncableProjectByID :many
SELECT id::text AS id, repository_url
FROM app.projects
WHERE enabled AND id = $1;

-- name: ListProjectsDueForSync :many
-- Used by the unattended sync loop only (SyncProjects): a project whose last
-- failure set app.issue_sync_state.next_retry_at in the future (see
-- UpsertIssueSyncStateFailure's exponential backoff) is skipped so a
-- repository with a revoked token or a deleted remote is not hammered on
-- every tick forever. The operator-triggered "Sync now" path (SyncNow) goes
-- through ListSyncableProjects/ListSyncableProjectByID instead and always
-- bypasses backoff, since a human explicitly asking for a sync right now is
-- exactly the case backoff should not stand in front of.
SELECT p.id::text AS id, p.repository_url
FROM app.projects p
LEFT JOIN app.issue_sync_state s ON s.project_id = p.id
WHERE p.enabled AND (s.next_retry_at IS NULL OR s.next_retry_at <= now());

-- name: CountProjectIssues :one
SELECT COUNT(*) FROM app.issues WHERE project_id = $1;

-- name: IssueSyncStatusEntries :many
-- eligible_count matches the scheduler's own candidate set (ListQueueEntries,
-- ClaimSchedulableIssue), not the bare label bit: an issue opted in by label
-- but sitting on a failed, blocked or delivered run that has not been
-- superseded by a retry is not actually schedulable, and the sidebar badge
-- would otherwise overcount it.
SELECT p.id::text AS id, p.name, p.enabled, COUNT(i.id) AS issue_count,
       COUNT(i.id) FILTER(WHERE i.eligible AND i.state = 'open' AND NOT EXISTS (
         SELECT 1 FROM app.workflow_runs w
         WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed', 'blocked', 'completed')
       )) AS eligible_count,
       s.last_synced_at, s.consecutive_failures, s.next_retry_at, s.last_error
FROM app.projects p
LEFT JOIN app.issues i ON i.project_id = p.id
LEFT JOIN app.issue_sync_state s ON s.project_id = p.id
GROUP BY p.id, p.name, p.enabled, s.last_synced_at, s.consecutive_failures, s.next_retry_at, s.last_error
ORDER BY p.name, p.id;

-- name: UpsertIssue :exec
-- eligible is written unconditionally from the tracker's labels (see
-- issuePriority) on every sync, full stop -- it is not the scheduler's
-- candidate signal by itself any more (see ListQueueEntries and
-- ClaimSchedulableIssue's app.workflow_runs.superseded_at join for that), so
-- there is no lifecycle state left here for a sync to clobber. See #268 and
-- migration 023 for why a CASE guarding this used to live here.
--
-- state is now written from what GitHub itself reports (ListIssues fetches
-- --state all, not just open) rather than being hardcoded to 'open': every
-- scheduling query (ListQueueEntries, GetSchedulerSnapshot,
-- ClaimSchedulableIssue) already filters on i.state = 'open', so an issue
-- closed on the tracker stops being schedulable the moment this reconciles
-- it, with no separate lifecycle write required. The caller also ANDs
-- eligible with "still open on the tracker", so a closed issue is reported
-- ineligible too, not just excluded via state.
INSERT INTO app.issues(id, project_id, provider, external_id, display_number, title, body, url, state, labels, priority, eligible, external_created_at, external_updated_at, last_synced_at, raw_snapshot)
VALUES ($1, $2, 'github', $3, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12, now(), $13::jsonb)
ON CONFLICT(project_id, provider, external_id) DO UPDATE SET
  display_number = EXCLUDED.display_number,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  url = EXCLUDED.url,
  state = EXCLUDED.state,
  labels = EXCLUDED.labels,
  priority = EXCLUDED.priority,
  eligible = EXCLUDED.eligible,
  external_created_at = EXCLUDED.external_created_at,
  external_updated_at = EXCLUDED.external_updated_at,
  last_synced_at = now(),
  raw_snapshot = EXCLUDED.raw_snapshot;

-- name: UpsertIssueSyncStateSuccess :exec
-- next_retry_at is reset to NULL on success: leaving a stale future timestamp
-- around after a project recovers would keep ListProjectsDueForSync skipping
-- it long after there is anything to back off from.
INSERT INTO app.issue_sync_state(project_id, consecutive_failures, next_retry_at, last_error, last_synced_at, updated_at)
VALUES ($1, 0, NULL, NULL, now(), now())
ON CONFLICT(project_id) DO UPDATE SET
  consecutive_failures = 0,
  next_retry_at = NULL,
  last_error = NULL,
  last_synced_at = now(),
  updated_at = now();

-- name: UpsertIssueSyncStateFailure :exec
-- next_retry_at backs off exponentially with consecutive_failures (1 minute,
-- 2, 4, 8, ...), capped at 1 hour. The exponent itself is capped (via the
-- inner LEAST) before POWER ever sees it, rather than relying on the outer
-- LEAST against '1 hour' alone: POWER(2, n) is evaluated first, and an
-- uncapped n for a project that has been failing for days would overflow
-- double precision (and then interval multiplication) before that outer
-- LEAST ever got a chance to clamp the result.
INSERT INTO app.issue_sync_state(project_id, consecutive_failures, next_retry_at, last_error, updated_at)
VALUES ($1, 1, now() + LEAST(INTERVAL '1 minute' * POWER(2, LEAST(1, 10)), INTERVAL '1 hour'), $2, now())
ON CONFLICT(project_id) DO UPDATE SET
  consecutive_failures = app.issue_sync_state.consecutive_failures + 1,
  next_retry_at = now() + LEAST(INTERVAL '1 minute' * POWER(2, LEAST(app.issue_sync_state.consecutive_failures + 1, 10)), INTERVAL '1 hour'),
  last_error = EXCLUDED.last_error,
  updated_at = now();
