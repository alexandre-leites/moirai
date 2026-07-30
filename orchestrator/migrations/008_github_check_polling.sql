ALTER TABLE app.workflow_runs
  ADD COLUMN IF NOT EXISTS github_check_poll_attempts INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS workflow_runs_github_check_polling_idx
  ON app.workflow_runs (updated_at, id)
  WHERE status = 'waiting_github_checks';
