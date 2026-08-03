-- name: UpsertProjectCredential :execrows
INSERT INTO app.project_credentials(project_id, kind, ciphertext, nonce, file_path)
SELECT $1, $2, $3, $4, $5
WHERE EXISTS (SELECT 1 FROM app.projects WHERE id = $1)
ON CONFLICT (project_id, kind) DO UPDATE SET
  ciphertext = EXCLUDED.ciphertext,
  nonce = EXCLUDED.nonce,
  file_path = EXCLUDED.file_path,
  updated_at = now();

-- name: DeleteProjectCredential :execrows
DELETE FROM app.project_credentials WHERE project_id = $1 AND kind = $2;

-- name: GetProjectCredentialSecret :one
SELECT ciphertext, nonce, file_path
FROM app.project_credentials
WHERE project_id = $1 AND kind = $2;

-- name: UpdateProjectCredentialSecret :execrows
UPDATE app.project_credentials
SET ciphertext = $3, nonce = $4, updated_at = now()
WHERE project_id = $1 AND kind = $2;

-- name: GetFencedJobProject :one
SELECT project_id::text AS project_id
FROM app.jobs
WHERE id = sqlc.arg(job_id)::uuid AND runner_id = sqlc.arg(runner_id)::uuid
  AND lease_generation = sqlc.arg(lease_generation)
  AND status IN ('preparing', 'running') AND lease_expires_at > now();

-- name: ProjectExists :one
SELECT EXISTS(SELECT 1 FROM app.projects WHERE id = $1) AS exists;

-- name: ListProjectCredentials :many
SELECT kind, created_at, updated_at, file_path
FROM app.project_credentials
WHERE project_id = $1
ORDER BY kind;
