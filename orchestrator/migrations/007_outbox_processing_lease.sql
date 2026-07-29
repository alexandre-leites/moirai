-- `processing` becomes a lease rather than a terminal state: a drainer records
-- when it claimed the row, and any drainer may take back a claim older than the
-- lease. Without it a process that died between claiming a row and delivering
-- it left that transition in `processing` forever, and the drain query only
-- ever selected `pending` (issue #96).
ALTER TABLE app.workflow_transition_outbox
  ADD COLUMN IF NOT EXISTS processing_started_at TIMESTAMPTZ;

-- Rows claimed by a build that could not record a claim time. Migrations run at
-- startup, before this process drains anything, so every such row is stranded
-- by definition: hand them back to the drain. Redelivery is safe -- the graph
-- reuses the execution request a first delivery already queued.
UPDATE app.workflow_transition_outbox
SET status = 'pending'
WHERE status = 'processing' AND processing_started_at IS NULL;

-- The drain now scans reclaimable `processing` rows alongside `pending` ones,
-- which the old pending-only partial index could not serve.
DROP INDEX IF EXISTS app.workflow_transition_outbox_pending_idx;
CREATE INDEX IF NOT EXISTS workflow_transition_outbox_drainable_idx
  ON app.workflow_transition_outbox (status, created_at)
  WHERE status IN ('pending', 'processing');
