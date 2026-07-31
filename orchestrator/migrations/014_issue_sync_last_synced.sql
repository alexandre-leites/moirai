ALTER TABLE app.issue_sync_state
  ADD COLUMN IF NOT EXISTS last_synced_at TIMESTAMPTZ;
