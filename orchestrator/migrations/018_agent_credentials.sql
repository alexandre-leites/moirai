-- Agent credentials: a provider key or a subscription credentials file, stored
-- per project beside the two git kinds 015 created.
--
-- Two changes, both to app.project_credentials:
--
-- 1. The kind CHECK is widened from the two fixed git kinds to also accept
--    `agent:<NAME>`, where <NAME> is the environment variable the value is
--    delivered under (OPENROUTER_API_KEY, ANTHROPIC_API_KEY, ...). The name is
--    chosen by whoever configures the project, so it cannot be enumerated here;
--    the pattern is the same one domain/credentials.py enforces, kept in both
--    places deliberately so a value that bypassed the application still cannot
--    reach a shape the runner would not understand.
--
-- 2. `file_path` says how the value has to arrive. Empty means an environment
--    variable. Non-empty is a path *relative to the throwaway home directory
--    the runner builds for the execution* -- the runner overrides HOME, so a
--    subscription harness looking for ~/.config/... finds nothing unless the
--    credential is placed under that home. The path is not secret; only the
--    ciphertext is.
--
-- The git kinds are unaffected: they keep an empty file_path and their delivery
-- keeps coming from the kind (a private key is always a file, a token never).
ALTER TABLE app.project_credentials DROP CONSTRAINT IF EXISTS project_credentials_kind_check;

ALTER TABLE app.project_credentials
  ADD CONSTRAINT project_credentials_kind_check
  CHECK (
    kind IN ('github_token', 'ssh_private_key')
    OR kind ~ '^agent:[A-Z_][A-Z0-9_]{0,127}$'
  );

ALTER TABLE app.project_credentials
  ADD COLUMN IF NOT EXISTS file_path TEXT NOT NULL DEFAULT '';

-- A file destination only means anything for an agent credential, and it must
-- stay inside the home directory the runner owns: no absolute path, no parent
-- traversal, no whitespace to confuse a shell that should never see it anyway.
ALTER TABLE app.project_credentials DROP CONSTRAINT IF EXISTS project_credentials_file_path_check;

ALTER TABLE app.project_credentials
  ADD CONSTRAINT project_credentials_file_path_check
  CHECK (
    file_path = ''
    OR (
      kind LIKE 'agent:%'
      AND length(file_path) <= 256
      AND file_path !~ '(^[/~]|\\|\s|(^|/)\.{1,2}(/|$)|//|/$)'
    )
  );
