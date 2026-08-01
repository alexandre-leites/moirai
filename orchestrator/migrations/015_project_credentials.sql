-- Per-project credentials, encrypted at rest.
--
-- Values are sealed with AES-GCM by persistence/secrets.py before they reach
-- this table and are never returned to any caller: the read path reports only
-- that a credential exists and when it last changed. The nonce is stored beside
-- the ciphertext because AES-GCM needs it to open, and it is not secret.
--
-- One row per (project, kind). Replacing a credential is an upsert, so a
-- project never accumulates stale versions of the same secret.
CREATE TABLE IF NOT EXISTS app.project_credentials (
  project_id UUID NOT NULL REFERENCES app.projects(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  ciphertext BYTEA NOT NULL,
  nonce BYTEA NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, kind),
  CHECK (kind IN ('github_token', 'ssh_private_key'))
);
