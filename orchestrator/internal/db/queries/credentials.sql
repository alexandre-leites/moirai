-- name: UpsertProjectCredential :execrows
-- Project-level credential (task_source_id NULL): the only kind the API
-- exposes today (SetProjectCredential/ClearProjectCredential/
-- ListProjectCredentials never take a source id -- see #293's PR
-- description). Targets the partial unique index migration 026 added for
-- exactly this row shape.
INSERT INTO app.project_credentials(project_id, kind, ciphertext, nonce, file_path)
SELECT $1, $2, $3, $4, $5
WHERE EXISTS (SELECT 1 FROM app.projects WHERE id = $1)
ON CONFLICT (project_id, kind) WHERE task_source_id IS NULL DO UPDATE SET
  ciphertext = EXCLUDED.ciphertext,
  nonce = EXCLUDED.nonce,
  file_path = EXCLUDED.file_path,
  updated_at = now();

-- name: UpsertProjectCredentialForSource :execrows
-- Source-scoped credential (task_source_id set): what lets two task sources
-- of the same provider each hold their own secret of a given kind, rather
-- than colliding on the one project-level slot. Not wired to a gRPC RPC yet
-- (see #293's PR description); exercised directly by
-- TestProjectCredentialSourceScopingPreventsCollision to prove the schema
-- decision actually holds.
INSERT INTO app.project_credentials(project_id, kind, task_source_id, ciphertext, nonce, file_path)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM app.projects WHERE id = $1)
ON CONFLICT (project_id, kind, task_source_id) WHERE task_source_id IS NOT NULL DO UPDATE SET
  ciphertext = EXCLUDED.ciphertext,
  nonce = EXCLUDED.nonce,
  file_path = EXCLUDED.file_path,
  updated_at = now();

-- name: DeleteProjectCredential :execrows
DELETE FROM app.project_credentials WHERE project_id = $1 AND kind = $2 AND task_source_id IS NULL;

-- name: DeleteProjectCredentialForSource :execrows
DELETE FROM app.project_credentials WHERE project_id = $1 AND kind = $2 AND task_source_id = $3;

-- name: GetProjectCredentialSecret :one
-- task_source_id (empty string = NULL, the same NULLIF convention CreateProject
-- uses for repository_url) is the caller's scope: a CodeHost lookup always
-- passes "" (a project has 0..1 code hosts, never per-source -- see
-- #291/#293), so only a project-level row (task_source_id IS NULL) can ever
-- match the OR's second arm for it, since the first arm compares against a
-- NULL and can never be true. A TaskSource lookup passes its own source id
-- and prefers a credential scoped to that source (the ORDER BY puts it
-- first when both exist), falling back to the project-level one when the
-- source has none of its own -- which is exactly what keeps an existing
-- single-source project's credential (set before this migration,
-- necessarily project-level) working unchanged after 026.
SELECT ciphertext, nonce, file_path
FROM app.project_credentials
WHERE project_id = $1 AND kind = $2
  AND (task_source_id = NULLIF(sqlc.arg(task_source_id)::text, '')::uuid OR task_source_id IS NULL)
ORDER BY (task_source_id IS NOT NULL) DESC
LIMIT 1;

-- name: UpdateProjectCredentialSecret :execrows
UPDATE app.project_credentials
SET ciphertext = $3, nonce = $4, updated_at = now()
WHERE project_id = $1 AND kind = $2 AND task_source_id IS NULL;

-- name: GetFencedJobProject :one
SELECT project_id::text AS project_id
FROM app.jobs
WHERE id = sqlc.arg(job_id)::uuid AND runner_id = sqlc.arg(runner_id)::uuid
  AND lease_generation = sqlc.arg(lease_generation)
  AND status IN ('preparing', 'running') AND lease_expires_at > now();

-- name: ProjectExists :one
SELECT EXISTS(SELECT 1 FROM app.projects WHERE id = $1) AS exists;

-- name: ListProjectCredentials :many
-- Lists every credential configured for the project, project-level and
-- source-scoped alike; task_source_id (nullable) is what tells them apart.
-- The gRPC surface (ListProjectCredentials/ProjectCredential) only ever
-- writes project-level rows today, so in practice this is empty for every
-- source-scoped row until a caller uses UpsertProjectCredentialForSource.
SELECT kind, created_at, updated_at, file_path, COALESCE(task_source_id::text, '') AS task_source_id
FROM app.project_credentials
WHERE project_id = $1
ORDER BY kind, task_source_id NULLS FIRST;
