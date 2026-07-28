ALTER TABLE app.workflow_runs
  ADD COLUMN IF NOT EXISTS last_diff_hash TEXT,
  ADD COLUMN IF NOT EXISTS non_progress_attempts INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS app.project_circuit_state (
  project_id UUID PRIMARY KEY REFERENCES app.projects(id) ON DELETE CASCADE,
  state TEXT NOT NULL DEFAULT 'closed' CHECK (state IN ('closed', 'open', 'half_open')),
  consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
  last_failure_reason TEXT,
  opened_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app.provider_circuit_state (
  provider TEXT PRIMARY KEY,
  state TEXT NOT NULL DEFAULT 'closed' CHECK (state IN ('closed', 'open', 'half_open')),
  consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
  last_failure_reason TEXT,
  opened_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS project_circuit_open_idx
  ON app.project_circuit_state (project_id)
  WHERE state = 'open';
