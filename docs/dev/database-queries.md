# Database helper queries for test data

Handy SQL for manipulating or inspecting test data in the orchestrator's
PostgreSQL database. Everything here is meant for a local/dev database, not
production.

## Connecting with psql

The database runs in the `postgres` service of `compose.yaml`. From the host
machine, connect with:

```bash
psql "postgresql://loop:moirai-local-postgres@localhost:5432/loop"
```

If the password was overridden, use the value of `MOIRAI_POSTGRES_PASSWORD`
(see `.env.example`). Inside the container the host is `postgres` instead of
`localhost`.

All queries below are written to be pasted into that psql session. They use
`app.`-qualified table names.

## Schema overview

Everything lives in the `app` schema. The tables that matter for test data:

- `app.projects` — the project; most other tables hang off it.
- `app.project_task_sources` — one or more issue sources per project.
- `app.issues` — synced issues, one row per issue.
- `app.workflow_runs` — one row per workflow run (the thing that processes an
  issue).
- `app.jobs` / `app.job_offers` / `app.executions` — runner work under a run.
- `app.project_locks` — the one-active-workflow-per-project lock.
- `app.workflow_events`, `app.workflow_transition_outbox`,
  `app.workflow_checkpoints`, `app.pipeline_runs`, `app.ai_reviews`,
  `app.pull_requests`, `app.human_approvals` — per-run detail.

Most per-run tables cascade-delete from `app.workflow_runs`, and most
per-project tables cascade-delete from `app.projects`. The exceptions that
must be deleted explicitly are `app.issues`, `app.workflow_runs`,
`app.jobs`, `app.project_locks` and `app.issue_sync_state` — they reference
their parent without `ON DELETE CASCADE`.

## Delete a project and everything related

Deletes a project by name, all its task sources, issues, workflow runs, jobs,
locks and every per-run detail. Wrapped in a transaction so a partial failure
leaves nothing half-deleted.

```sql
BEGIN;

-- Capture the project id from its name.
WITH target AS (
  SELECT id FROM app.projects WHERE name = 'my-project'
)
-- Per-run detail (cascades from workflow_runs) and jobs first.
DELETE FROM app.jobs WHERE project_id IN (SELECT id FROM target);
DELETE FROM app.workflow_runs WHERE project_id IN (SELECT id FROM target);
DELETE FROM app.project_locks WHERE project_id IN (SELECT id FROM target);
-- Issues and their sync state.
DELETE FROM app.issues WHERE project_id IN (SELECT id FROM target);
DELETE FROM app.issue_sync_state WHERE project_id IN (SELECT id FROM target);
-- Task sources (cascades issues.task_source_id and credentials).
DELETE FROM app.project_task_sources WHERE project_id IN (SELECT id FROM target);
-- The project itself (cascades labels, pipeline steps, credentials, circuit state).
DELETE FROM app.projects WHERE id IN (SELECT id FROM target);

COMMIT;
```

Replace `PROJECT_NAME` with the actual project name. To delete every project
instead, drop the `WHERE` clauses (or use `WHERE true`).

## Delete only the workflows of a project

Removes all workflow runs (and their jobs, locks, events, checkpoints,
reviews, pull requests, outbox rows) for a project, but keeps the project,
its task sources and its issues. Useful to reset a project's run history
without losing its configuration.

```sql
BEGIN;

WITH target AS (
  SELECT id FROM app.projects WHERE name = 'PROJECT_NAME'
)
DELETE FROM app.jobs WHERE project_id IN (SELECT id FROM target);
DELETE FROM app.workflow_runs WHERE project_id IN (SELECT id FROM target);
DELETE FROM app.project_locks WHERE project_id IN (SELECT id FROM target);

COMMIT;
```

## Delete a single workflow run

Delete one run by its id. Per-run detail cascades; jobs and the project lock
must be removed explicitly.

```sql
BEGIN;

DELETE FROM app.jobs WHERE workflow_run_id = 'RUN_ID';
DELETE FROM app.project_locks WHERE workflow_run_id = 'RUN_ID';
DELETE FROM app.workflow_runs WHERE id = 'RUN_ID';

COMMIT;
```

Find a run id with:

```sql
SELECT id, project_id, status, current_phase, created_at
FROM app.workflow_runs
ORDER BY created_at DESC
LIMIT 20;
```

## Change a workflow's status

Set a run's status directly. Useful to unstick a run or simulate a terminal
state. The `status` column is CHECK-constrained to the known vocabulary:
`offered`, `preparing`, `planning`, `waiting_github_checks`, `delivering`,
`waiting_human`, `waiting_ai_review`, `repairing`, `pipeline_failed`,
`completed`, `failed`, `blocked`, `cancelled`.

```sql
UPDATE app.workflow_runs
SET status = 'cancelled', updated_at = now()
WHERE id = 'RUN_ID';
```

To cancel every non-terminal run of a project:

```sql
UPDATE app.workflow_runs
SET status = 'cancelled', updated_at = now()
WHERE project_id = (SELECT id FROM app.projects WHERE name = 'PROJECT_NAME')
  AND status NOT IN ('completed', 'failed', 'blocked', 'cancelled');
```

## Release a stuck project lock

If a run is gone but its lock row remains (or you want to free a project
manually), delete the lock:

```sql
DELETE FROM app.project_locks WHERE project_id = 'PROJECT_ID';
```

## Inspect current state

List projects:

```sql
SELECT id, name, enabled, repository_mode, repository_url, local_repository_path
FROM app.projects
ORDER BY name;
```

List workflow runs with their project and issue:

```sql
SELECT w.id, p.name AS project, w.status, w.current_phase, w.created_at
FROM app.workflow_runs w
JOIN app.projects p ON p.id = w.project_id
ORDER BY w.created_at DESC
LIMIT 50;
```

List active (non-terminal) runs:

```sql
SELECT w.id, p.name, w.status, w.current_phase
FROM app.workflow_runs w
JOIN app.projects p ON p.id = w.project_id
WHERE w.status NOT IN ('completed', 'failed', 'blocked', 'cancelled');
```

List current project locks:

```sql
SELECT l.project_id, p.name, l.workflow_run_id, l.acquired_at
FROM app.project_locks l
JOIN app.projects p ON p.id = l.project_id;
```

List jobs and their runner:

```sql
SELECT j.id, j.status, j.lease_generation, j.lease_expires_at, r.name AS runner
FROM app.jobs j
LEFT JOIN app.runners r ON r.id = j.runner_id
ORDER BY j.offered_at DESC
LIMIT 50;
```

## Notes

- Always run the multi-statement deletes inside a transaction (`BEGIN` /
  `COMMIT`) so a failure rolls back cleanly.
- These queries bypass the application entirely. The orchestrator caches some
  state in memory, so after a manual change you may need to restart the
  orchestrator (or wait for its next poll) for the UI to reflect it.
- Deleting a project does not touch `app.runners` or `app.users`; those are
  global and shared across projects.