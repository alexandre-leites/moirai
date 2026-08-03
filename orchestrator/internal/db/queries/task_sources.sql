-- Task sources (#293): the first-class row a project's task sources live
-- in, re-keying what used to be app.projects.issue_tracker_type (a single
-- scalar, dropped by migration 026) onto app.project_task_sources (1..N per
-- project, 0 valid).
--
-- Only a read query is defined here for now. Creating, editing, enabling and
-- deleting a source needs the field-level descriptor #294 introduces (which
-- kind of source needs which configuration fields, and which of them are
-- secrets) to have a real write API rather than a hand-rolled one #294 would
-- have to redesign around anyway -- see the PR description for #293. Every
-- project already has at least its migrated default source from 026, so a
-- read-only surface is enough to make what is configured visible.

-- name: ListProjectTaskSources :many
SELECT id::text AS id, project_id::text AS project_id, provider, name, enabled, configuration::text AS configuration, created_at, updated_at
FROM app.project_task_sources
WHERE project_id = $1
ORDER BY name, id;
