-- name: ListProjectIDs :many
SELECT id::text AS id FROM app.projects ORDER BY name, id;

-- name: ListEnabledProjectIDs :many
-- SyncNow's "sync every project" path (no project_id given): the set of
-- projects whose task sources are even candidates for ListSyncableSources.
SELECT id::text AS id FROM app.projects WHERE enabled ORDER BY name, id;

-- name: ProjectIsEnabled :one
-- Distinguishes "unknown project" (no row, pgx.ErrNoRows) from "disabled
-- project" (a row, enabled = false) for SyncNow's single-project path, which
-- reports both as the same 404 -- syncing a specific project only ever makes
-- sense for one that is both known and enabled.
SELECT enabled FROM app.projects WHERE id = $1;

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
SELECT id::text AS id, name, enabled, repository_mode, repository_url, local_repository_path, default_branch, configuration::text AS configuration, code_host_type
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

-- name: ListSyncableSources :many
-- One row per enabled task source of every enabled project -- the unit the
-- sync loop now iterates is a source, not a project (see #293): a project
-- with zero configured sources simply contributes no rows here, and a
-- disabled source is skipped the same way a disabled project always was.
SELECT ts.id::text AS id, ts.project_id::text AS project_id, ts.provider, ts.configuration::text AS configuration
FROM app.project_task_sources ts
JOIN app.projects p ON p.id = ts.project_id
WHERE p.enabled AND ts.enabled;

-- name: ListSyncableSourcesByProject :many
SELECT ts.id::text AS id, ts.project_id::text AS project_id, ts.provider, ts.configuration::text AS configuration
FROM app.project_task_sources ts
JOIN app.projects p ON p.id = ts.project_id
WHERE p.enabled AND ts.enabled AND ts.project_id = $1;

-- name: ListSourcesDueForSync :many
-- Used by the unattended sync loop only (SyncProjects): a source whose last
-- failure set app.issue_sync_state.next_retry_at in the future (see
-- UpsertIssueSyncStateFailure's exponential backoff) is skipped so a
-- repository with a revoked token or a deleted remote is not hammered on
-- every tick forever. The operator-triggered "Sync now" path (SyncNow) goes
-- through ListSyncableSources/ListSyncableSourcesByProject instead and always
-- bypasses backoff, since a human explicitly asking for a sync right now is
-- exactly the case backoff should not stand in front of. Backoff is tracked
-- per source (app.issue_sync_state is keyed by task_source_id), so one
-- source's failure no longer throttles a healthy sibling source on the same
-- project.
SELECT ts.id::text AS id, ts.project_id::text AS project_id, ts.provider, ts.configuration::text AS configuration
FROM app.project_task_sources ts
JOIN app.projects p ON p.id = ts.project_id
LEFT JOIN app.issue_sync_state s ON s.task_source_id = ts.id
WHERE p.enabled AND ts.enabled AND (s.next_retry_at IS NULL OR s.next_retry_at <= now());

-- name: CountProjectIssues :one
SELECT COUNT(*) FROM app.issues WHERE project_id = $1;

-- name: IssueSyncStatusEntries :many
-- eligible_count matches the scheduler's own candidate set (ListQueueEntries,
-- ClaimSchedulableIssue), not the bare label bit: an issue opted in by label
-- but sitting on a failed, blocked or delivered run that has not been
-- superseded by a retry is not actually schedulable, and the sidebar badge
-- would otherwise overcount it.
--
-- This reports one row per *project*, aggregated across however many task
-- sources it has, rather than one row per source: the console's sync panel
-- (out of scope for #293 -- see the PR description) reads this shape today,
-- and a project with its one auto-migrated source produces exactly the same
-- row it always did. last_synced_at/consecutive_failures take the MAX across
-- a project's sources (the worst case is the one worth surfacing),
-- next_retry_at the soonest non-null retry, and last_error the most
-- recently updated source's error. A true per-source breakdown is future
-- work once the console has somewhere to show it.
SELECT p.id::text AS id, p.name, p.enabled, COUNT(i.id) AS issue_count,
       COUNT(i.id) FILTER(WHERE i.eligible AND i.state = 'open' AND NOT EXISTS (
         SELECT 1 FROM app.workflow_runs w
         WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed', 'blocked', 'completed')
       )) AS eligible_count,
       MAX(s.last_synced_at)::timestamptz AS last_synced_at,
       -- COALESCEd to 0/'' rather than left NULL: a project with zero
       -- configured task sources has no per-source failure/error to report
       -- (there is nothing failing), which is the same "nothing to show"
       -- state a project whose one source has never failed already reports.
       COALESCE(MAX(s.consecutive_failures), 0)::int AS consecutive_failures,
       MIN(s.next_retry_at)::timestamptz AS next_retry_at,
       COALESCE((array_agg(s.last_error ORDER BY s.updated_at DESC NULLS LAST) FILTER (WHERE s.last_error IS NOT NULL))[1], '')::text AS last_error
FROM app.projects p
LEFT JOIN app.project_task_sources ts ON ts.project_id = p.id
LEFT JOIN app.issues i ON i.task_source_id = ts.id
LEFT JOIN app.issue_sync_state s ON s.task_source_id = ts.id
GROUP BY p.id, p.name, p.enabled
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
-- The DO UPDATE's WHERE guard (added for #290) is what keeps a steady-state
-- sync pass -- nothing changed on the tracker since the last one -- from
-- rewriting every issue row, full raw_snapshot JSONB included, on every
-- single pass. Without it, Postgres has no way to know the SET list would
-- produce an identical row and pays a full tuple write (and a fresh xmin,
-- and a dead tuple for autovacuum to reclaim) regardless. The comparison
-- covers every column the SET list actually writes from EXCLUDED except
-- last_synced_at itself (which always advances and would otherwise defeat
-- the guard by always differing); a row with no other change simply keeps
-- its previous last_synced_at rather than that column alone forcing a write.
INSERT INTO app.issues(id, project_id, task_source_id, provider, external_id, display_number, title, body, url, state, labels, priority, eligible, external_created_at, external_updated_at, last_synced_at, raw_snapshot)
VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14, now(), $15::jsonb)
ON CONFLICT(task_source_id, external_id) DO UPDATE SET
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
  raw_snapshot = EXCLUDED.raw_snapshot
WHERE
  app.issues.display_number IS DISTINCT FROM EXCLUDED.display_number OR
  app.issues.title IS DISTINCT FROM EXCLUDED.title OR
  app.issues.body IS DISTINCT FROM EXCLUDED.body OR
  app.issues.url IS DISTINCT FROM EXCLUDED.url OR
  app.issues.state IS DISTINCT FROM EXCLUDED.state OR
  app.issues.labels IS DISTINCT FROM EXCLUDED.labels OR
  app.issues.priority IS DISTINCT FROM EXCLUDED.priority OR
  app.issues.eligible IS DISTINCT FROM EXCLUDED.eligible OR
  app.issues.external_created_at IS DISTINCT FROM EXCLUDED.external_created_at OR
  app.issues.external_updated_at IS DISTINCT FROM EXCLUDED.external_updated_at OR
  app.issues.raw_snapshot IS DISTINCT FROM EXCLUDED.raw_snapshot;

-- name: UpsertIssueSyncStateSuccess :exec
-- next_retry_at is reset to NULL on success: leaving a stale future timestamp
-- around after a source recovers would keep ListSourcesDueForSync skipping
-- it long after there is anything to back off from. Keyed by task_source_id
-- (see migration 026): a project's sources back off independently, so one
-- source's failure streak cannot mask or reset alongside a sibling's success.
INSERT INTO app.issue_sync_state(task_source_id, project_id, consecutive_failures, next_retry_at, last_error, last_synced_at, updated_at)
VALUES ($1, $2, 0, NULL, NULL, now(), now())
ON CONFLICT(task_source_id) DO UPDATE SET
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
-- uncapped n for a source that has been failing for days would overflow
-- double precision (and then interval multiplication) before that outer
-- LEAST ever got a chance to clamp the result.
INSERT INTO app.issue_sync_state(task_source_id, project_id, consecutive_failures, next_retry_at, last_error, updated_at)
VALUES ($1, $3, 1, now() + LEAST(INTERVAL '1 minute' * POWER(2, LEAST(1, 10)), INTERVAL '1 hour'), $2, now())
ON CONFLICT(task_source_id) DO UPDATE SET
  consecutive_failures = app.issue_sync_state.consecutive_failures + 1,
  next_retry_at = now() + LEAST(INTERVAL '1 minute' * POWER(2, LEAST(app.issue_sync_state.consecutive_failures + 1, 10)), INTERVAL '1 hour'),
  last_error = EXCLUDED.last_error,
  updated_at = now();
