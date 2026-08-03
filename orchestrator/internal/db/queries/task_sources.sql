-- Task sources (#293): the first-class row a project's task sources live
-- in, re-keying what used to be app.projects.issue_tracker_type (a single
-- scalar, dropped by migration 026) onto app.project_task_sources (1..N per
-- project, 0 valid).
--
-- #294 adds the field-level descriptor (which configuration fields a
-- provider needs, and which of them are secrets) that makes a real write API
-- possible: CreateProjectTaskSource/UpdateProjectTaskSource/
-- DeleteProjectTaskSource below, each validated server-side against that
-- descriptor before ever reaching this file (see descriptor.go and
-- tasksources_rpc.go). Every project already has at least its migrated
-- default source from 026, so a project can never end up with a task source
-- whose provider this orchestrator has no descriptor for -- except that one
-- migrated row, which predates #294 and is left alone.

-- name: ListProjectTaskSources :many
SELECT id::text AS id, project_id::text AS project_id, provider, name, enabled, configuration::text AS configuration, created_at, updated_at
FROM app.project_task_sources
WHERE project_id = $1
ORDER BY name, id;

-- name: GetProjectTaskSourceByID :one
-- Used by Update/DeleteTaskSource, which only receive a task_source_id (its
-- project isn't known to the caller yet); app.project_task_sources.id is a
-- global primary key so this is unambiguous, and every write those handlers
-- go on to make still scopes by the project_id this returns.
SELECT id::text AS id, project_id::text AS project_id, provider, name, enabled, configuration::text AS configuration, created_at, updated_at
FROM app.project_task_sources
WHERE id = $1;

-- name: GetProjectTaskSource :one
-- Scoped by project_id as well as id so an UpdateProjectTaskSource/
-- DeleteProjectTaskSource caller cannot act on a source belonging to a
-- different project by guessing its id.
SELECT id::text AS id, project_id::text AS project_id, provider, name, enabled, configuration::text AS configuration, created_at, updated_at
FROM app.project_task_sources
WHERE id = $1 AND project_id = $2;

-- name: CreateProjectTaskSource :exec
INSERT INTO app.project_task_sources (id, project_id, provider, name, enabled, configuration)
VALUES ($1, $2, $3, $4, $5, $6::jsonb);

-- name: UpdateProjectTaskSource :execrows
UPDATE app.project_task_sources
SET name = $3, enabled = $4, configuration = $5::jsonb, updated_at = now()
WHERE id = $1 AND project_id = $2;

-- name: DeleteProjectTaskSource :execrows
DELETE FROM app.project_task_sources WHERE id = $1 AND project_id = $2;
