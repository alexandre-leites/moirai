-- name: ListRunners :many
SELECT id::text AS id, name, enabled, draining, status, labels::text AS labels, last_seen_at, version
FROM app.runners
ORDER BY name, id;

-- name: GetRunner :one
SELECT id::text AS id, name, enabled, draining, status, labels::text AS labels, last_seen_at, version
FROM app.runners
WHERE id = $1;

-- name: DrainRunner :execrows
UPDATE app.runners SET draining = true WHERE id = $1 AND revoked_at IS NULL;

-- name: EnableRunner :execrows
UPDATE app.runners SET enabled = true, draining = false WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeRunner :execrows
UPDATE app.runners SET enabled = false, status = 'offline', revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeRunnerCredentials :exec
UPDATE app.runner_credentials SET revoked_at = now() WHERE runner_id = $1 AND revoked_at IS NULL;

-- name: SelectValidRegistrationToken :one
SELECT id::text AS id
FROM app.runner_registration_tokens
WHERE token_hash = $1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now() AND allowed_labels @> $2::jsonb
FOR UPDATE;

-- name: CreateRunner :exec
INSERT INTO app.runners (id, name, status, version, labels, capacity, last_seen_at)
VALUES ($1, $2, 'offline', '', $3::jsonb, $4, now());

-- name: CreateRunnerCredential :exec
INSERT INTO app.runner_credentials (id, runner_id, credential_hash)
VALUES ($1, $2, $3);

-- name: MarkRegistrationTokenUsed :exec
UPDATE app.runner_registration_tokens SET used_at = now() WHERE id = $1;

-- name: RecordRunnerHeartbeat :exec
UPDATE app.runners
SET status = 'online', last_seen_at = now(), version = COALESCE(NULLIF($2, ''), version)
WHERE id = $1 AND enabled AND revoked_at IS NULL;

-- name: SetRunnerDraining :exec
UPDATE app.runners SET draining = $2, last_seen_at = now() WHERE id = $1;
