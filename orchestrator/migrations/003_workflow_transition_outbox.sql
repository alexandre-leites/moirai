CREATE TABLE IF NOT EXISTS app.workflow_transition_outbox (
  id UUID PRIMARY KEY,
  workflow_run_id UUID NOT NULL REFERENCES app.workflow_runs(id) ON DELETE CASCADE,
  new_status TEXT NOT NULL,
  state_updates JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'processed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS workflow_transition_outbox_pending_idx
  ON app.workflow_transition_outbox (status, created_at)
  WHERE status = 'pending';
