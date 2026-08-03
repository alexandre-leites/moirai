-- #294: task-source secret fields (a GitHub source's own "token", per the
-- TaskSourceType descriptor in orchestrator/internal/server/descriptor.go)
-- route to this same table, source-scoped via migration 026's nullable
-- task_source_id column, under the kind namespace "source_field:<field
-- key>" (see tasksources_rpc.go's taskSourceSecretKind). The existing kind
-- CHECK (018) only accepts the two fixed git kinds or "agent:<NAME>", so a
-- source-scoped secret's kind needs its own arm here rather than silently
-- failing the insert at the database level.
ALTER TABLE app.project_credentials DROP CONSTRAINT IF EXISTS project_credentials_kind_check;

ALTER TABLE app.project_credentials
  ADD CONSTRAINT project_credentials_kind_check
  CHECK (
    kind IN ('github_token', 'ssh_private_key')
    OR kind ~ '^agent:[A-Z_][A-Z0-9_]{0,127}$'
    OR kind ~ '^source_field:[a-z][a-z0-9_]*$'
  );
