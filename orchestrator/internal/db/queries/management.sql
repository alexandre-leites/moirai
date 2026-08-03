-- name: GetUserForUpdate :one
SELECT username, password_hash, email, display_name
FROM app.users
WHERE id = $1
FOR UPDATE;

-- name: UpdateUserPassword :exec
UPDATE app.users
SET password_hash = $2, updated_at = now()
WHERE id = $1;

-- name: RevokeOtherUserSessions :exec
UPDATE app.user_sessions
SET revoked_at = now()
WHERE user_id = $1 AND token_hash <> $2 AND revoked_at IS NULL;

-- name: UpdateUserProfile :exec
UPDATE app.users
SET email = $2, display_name = $3, updated_at = now()
WHERE id = $1;

-- name: CreateRunnerRegistrationToken :one
INSERT INTO app.runner_registration_tokens(id, token_hash, created_by_user_id, allowed_labels, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(token_hash), sqlc.arg(created_by_user_id)::uuid, sqlc.arg(allowed_labels)::jsonb, now() + interval '15 minutes')
RETURNING expires_at;

-- name: ListRunnerRegistrationTokens :many
SELECT id::text AS id, allowed_labels::text AS allowed_labels, created_at, expires_at, used_at, revoked_at
FROM app.runner_registration_tokens
ORDER BY created_at DESC, id;

-- name: RevokeRunnerRegistrationToken :one
UPDATE app.runner_registration_tokens
SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1
RETURNING id::text AS id, allowed_labels::text AS allowed_labels, created_at, expires_at, used_at, revoked_at;

-- name: GetSessionCSRFHash :one
SELECT csrf_token_hash
FROM app.user_sessions
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now();
