CREATE SCHEMA IF NOT EXISTS app;
CREATE SCHEMA IF NOT EXISTS langgraph;

CREATE TABLE IF NOT EXISTS app.users (
  id UUID PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'admin' CHECK (role IN ('admin', 'viewer')),
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.user_sessions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES app.users(id),
  token_hash TEXT NOT NULL UNIQUE,
  csrf_token_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  last_seen_at TIMESTAMPTZ,
  client_metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS app.projects (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  enabled BOOLEAN NOT NULL DEFAULT true,
  repository_mode TEXT NOT NULL,
  repository_url TEXT,
  local_repository_path TEXT,
  default_branch TEXT NOT NULL DEFAULT 'main',
  issue_tracker_type TEXT NOT NULL DEFAULT 'github',
  code_host_type TEXT NOT NULL DEFAULT 'github',
  configuration JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (repository_mode IN ('managed_clone', 'existing_path')),
  CHECK ((repository_mode = 'managed_clone' AND repository_url IS NOT NULL AND local_repository_path IS NULL) OR (repository_mode = 'existing_path' AND local_repository_path IS NOT NULL AND repository_url IS NULL))
);

CREATE TABLE IF NOT EXISTS app.project_labels (
  project_id UUID PRIMARY KEY REFERENCES app.projects(id) ON DELETE CASCADE,
  ready_label TEXT NOT NULL DEFAULT 'agent:ready',
  running_label TEXT NOT NULL DEFAULT 'agent:running',
  review_label TEXT NOT NULL DEFAULT 'agent:review',
  blocked_label TEXT NOT NULL DEFAULT 'agent:blocked',
  human_required_label TEXT NOT NULL DEFAULT 'agent:human-required',
  human_approval_label TEXT NOT NULL DEFAULT 'agent:human-approval',
  failed_label TEXT NOT NULL DEFAULT 'agent:failed',
  delivered_label TEXT NOT NULL DEFAULT 'agent:delivered',
  priority_prefix TEXT NOT NULL DEFAULT 'agent-priority:',
  default_priority INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS app.project_pipeline_steps (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES app.projects(id) ON DELETE CASCADE,
  position INTEGER NOT NULL CHECK (position >= 0),
  name TEXT NOT NULL,
  command TEXT NOT NULL,
  timeout_seconds INTEGER NOT NULL CHECK (timeout_seconds > 0),
  required BOOLEAN NOT NULL DEFAULT true,
  environment JSONB NOT NULL DEFAULT '{}'::jsonb,
  UNIQUE (project_id, position)
);

CREATE TABLE IF NOT EXISTS app.issues (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES app.projects(id),
  provider TEXT NOT NULL,
  external_id TEXT NOT NULL,
  display_number TEXT NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL,
  state TEXT NOT NULL,
  labels JSONB NOT NULL DEFAULT '[]'::jsonb,
  priority INTEGER NOT NULL DEFAULT 0,
  eligible BOOLEAN NOT NULL DEFAULT false,
  human_approval_required BOOLEAN NOT NULL DEFAULT false,
  external_created_at TIMESTAMPTZ NOT NULL,
  external_updated_at TIMESTAMPTZ NOT NULL,
  last_synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  raw_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
  UNIQUE (project_id, provider, external_id)
);
CREATE INDEX IF NOT EXISTS issues_eligible_priority_idx ON app.issues (eligible, priority DESC, external_created_at, id);

CREATE TABLE IF NOT EXISTS app.issue_sync_state (
  project_id UUID PRIMARY KEY REFERENCES app.projects(id) ON DELETE CASCADE,
  consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
  next_retry_at TIMESTAMPTZ,
  last_error TEXT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.workflow_runs (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES app.projects(id),
  issue_id UUID NOT NULL REFERENCES app.issues(id),
  thread_id TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL,
  current_phase TEXT NOT NULL,
  branch_name TEXT,
  base_commit TEXT,
  current_commit TEXT,
  pull_request_external_id TEXT,
  pull_request_url TEXT,
  planning_attempts INTEGER NOT NULL DEFAULT 0,
  implementation_attempts INTEGER NOT NULL DEFAULT 0,
  pipeline_repair_attempts INTEGER NOT NULL DEFAULT 0,
  review_cycles INTEGER NOT NULL DEFAULT 0,
  ci_repair_attempts INTEGER NOT NULL DEFAULT 0,
  total_agent_executions INTEGER NOT NULL DEFAULT 0,
  last_progress_at TIMESTAMPTZ,
  last_failure_fingerprint TEXT,
  blocking_reason TEXT,
  terminal_reason TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.workflow_events (
  id BIGSERIAL PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  phase TEXT,
  severity TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workflow_events_run_created_idx ON app.workflow_events (workflow_run_id, created_at, id);

CREATE TABLE IF NOT EXISTS app.project_locks (
  project_id UUID PRIMARY KEY REFERENCES app.projects(id),
  workflow_run_id UUID NOT NULL UNIQUE REFERENCES app.workflow_runs(id),
  acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.runners (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  draining BOOLEAN NOT NULL DEFAULT false,
  status TEXT NOT NULL,
  version TEXT NOT NULL,
  labels JSONB NOT NULL DEFAULT '[]'::jsonb,
  capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
  last_seen_at TIMESTAMPTZ,
  registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.runner_credentials (
  id UUID PRIMARY KEY,
  runner_id UUID NOT NULL REFERENCES app.runners(id) ON DELETE CASCADE,
  credential_hash TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.runner_registration_tokens (
  id UUID PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  created_by_user_id UUID REFERENCES app.users(id),
  allowed_labels JSONB NOT NULL DEFAULT '[]'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  used_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.jobs (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL UNIQUE REFERENCES app.workflow_runs(id),
  project_id UUID NOT NULL REFERENCES app.projects(id),
  runner_id UUID REFERENCES app.runners(id),
  status TEXT NOT NULL,
  lease_generation BIGINT NOT NULL DEFAULT 0,
  last_event_sequence BIGINT NOT NULL DEFAULT 0,
  lease_expires_at TIMESTAMPTZ,
  offered_at TIMESTAMPTZ,
  accepted_at TIMESTAMPTZ,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  recovery_reason TEXT
);
CREATE INDEX IF NOT EXISTS jobs_runner_status_idx ON app.jobs (runner_id, status);

CREATE TABLE IF NOT EXISTS app.job_offers (
  id UUID PRIMARY KEY,
  job_id UUID NOT NULL REFERENCES app.jobs(id) ON DELETE CASCADE,
  runner_id UUID NOT NULL REFERENCES app.runners(id),
  status TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  responded_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.executions (
  id UUID PRIMARY KEY,
  job_id UUID NOT NULL REFERENCES app.jobs(id) ON DELETE CASCADE,
  execution_type TEXT NOT NULL,
  attempt INTEGER NOT NULL CHECK (attempt >= 0),
  status TEXT NOT NULL,
  lease_generation BIGINT NOT NULL,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  timeout_seconds INTEGER NOT NULL CHECK (timeout_seconds > 0),
  exit_code INTEGER,
  result JSONB,
  error JSONB,
  failure_fingerprint TEXT,
  UNIQUE (job_id, execution_type, attempt)
);

CREATE TABLE IF NOT EXISTS app.workflow_checkpoints (
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  version BIGINT NOT NULL CHECK (version > 0),
  state JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (workflow_run_id, version)
);

CREATE TABLE IF NOT EXISTS app.workflow_execution_requests (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  attempt INTEGER NOT NULL CHECK (attempt > 0),
  status TEXT NOT NULL DEFAULT 'queued',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  dispatched_at TIMESTAMPTZ,
  UNIQUE (workflow_run_id, role, attempt)
);
CREATE INDEX IF NOT EXISTS workflow_execution_requests_queued_idx
  ON app.workflow_execution_requests (status, created_at);

CREATE TABLE IF NOT EXISTS app.pipeline_runs (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  commit_sha TEXT NOT NULL,
  status TEXT NOT NULL,
  result JSONB NOT NULL DEFAULT '{}'::jsonb,
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS app.ai_reviews (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  commit_sha TEXT NOT NULL,
  verdict TEXT NOT NULL,
  result JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.pull_requests (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL UNIQUE REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  external_id TEXT NOT NULL,
  url TEXT NOT NULL,
  head_commit TEXT NOT NULL,
  state TEXT NOT NULL,
  merged_at TIMESTAMPTZ,
  raw_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS app.human_approvals (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  commit_sha TEXT NOT NULL,
  user_id UUID NOT NULL REFERENCES app.users(id),
  decision TEXT NOT NULL,
  comment TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.audit_events (
  id BIGSERIAL PRIMARY KEY,
  actor_type TEXT NOT NULL,
  actor_id TEXT,
  action TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
