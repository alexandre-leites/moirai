-- name: CountUsers :one
SELECT COUNT(*) FROM app.users;

-- name: CreateAdminUser :exec
INSERT INTO app.users(id, username, password_hash, role)
VALUES ($1, $2, $3, 'admin')
ON CONFLICT(username) DO NOTHING;

-- name: UpsertSeedRunnerRegistrationToken :exec
INSERT INTO app.runner_registration_tokens(id, token_hash, allowed_labels, expires_at)
VALUES ($1, $2, $3::jsonb, now() + interval '15 minutes')
ON CONFLICT(token_hash) DO UPDATE SET
  allowed_labels = EXCLUDED.allowed_labels,
  expires_at = EXCLUDED.expires_at
WHERE app.runner_registration_tokens.used_at IS NULL;

-- name: GetUserByUsername :one
SELECT id::text AS id, password_hash, enabled
FROM app.users
WHERE username = $1;

-- name: CreateUserSession :exec
INSERT INTO app.user_sessions(id, user_id, token_hash, csrf_token_hash, expires_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, now());

-- name: GetUserProfile :one
SELECT username, email, display_name
FROM app.users
WHERE id = $1;

-- name: RevokeSessionByTokens :execrows
UPDATE app.user_sessions
SET revoked_at = now()
WHERE token_hash = $1 AND csrf_token_hash = $2 AND revoked_at IS NULL AND expires_at > now();

-- name: GetSessionActor :one
SELECT u.id::text AS id, u.role, us.csrf_token_hash
FROM app.user_sessions us
JOIN app.users u ON u.id = us.user_id
WHERE us.token_hash = $1 AND us.revoked_at IS NULL AND us.expires_at > now() AND u.enabled;

-- name: GetRunnerCredentialHash :one
SELECT c.credential_hash
FROM app.runners r
JOIN app.runner_credentials c ON c.runner_id = r.id
WHERE r.id = $1 AND r.enabled AND r.revoked_at IS NULL AND c.revoked_at IS NULL
  AND (c.expires_at IS NULL OR c.expires_at > now())
ORDER BY c.created_at DESC
LIMIT 1;

-- name: CreateAuditEvent :exec
INSERT INTO app.audit_events(actor_type, actor_id, action, target_type, target_id)
VALUES ('user', $1, $2, $3, $4);
