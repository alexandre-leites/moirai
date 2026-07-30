ALTER TABLE app.workflow_runs
  ADD COLUMN IF NOT EXISTS continuation_attempts INTEGER NOT NULL DEFAULT 0 CHECK (continuation_attempts >= 0),
  ADD COLUMN IF NOT EXISTS last_delivery_outcome TEXT,
  ADD COLUMN IF NOT EXISTS last_gate_verdict TEXT,
  ADD COLUMN IF NOT EXISTS remaining_work JSONB NOT NULL DEFAULT '[]'::jsonb;
