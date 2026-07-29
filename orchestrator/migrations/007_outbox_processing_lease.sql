-- `processing` becomes a lease rather than a terminal state: a drainer records
-- when it claimed the row, and any drainer may take back a claim older than the
-- lease. Without it a process that died between claiming a row and delivering
-- it left that transition in `processing` forever, and the drain query only
-- ever selected `pending` (issue #96).
ALTER TABLE app.workflow_transition_outbox
  ADD COLUMN IF NOT EXISTS processing_started_at TIMESTAMPTZ;

-- Rows claimed by a build that had no column to record a claim time in. Once
-- this has run every claim stamps the column, so the predicate matches nothing
-- on a re-run. During a rolling deploy an older replica may still hold such a
-- row and be actively delivering it, so this can hand back a claim that is
-- still live -- which costs at most one redelivery, and the graph absorbs that
-- by reusing the execution request the first delivery already queued.
UPDATE app.workflow_transition_outbox
SET status = 'pending'
WHERE status = 'processing' AND processing_started_at IS NULL;

-- The drain scans reclaimable `processing` rows alongside `pending` ones, which
-- the old pending-only partial index could not serve. `created_at` alone is the
-- key: the predicate already selects the statuses, so leaving `status` out of
-- the key makes the index match the drain's own `ORDER BY created_at`, and its
-- `LIMIT` can then stop early on an ordered index scan rather than sorting
-- every drainable row. (On a near-empty table the planner still prefers a
-- bitmap scan and a sort; the ordered scan is what it picks once the backlog is
-- big enough for the difference to matter.)
DROP INDEX IF EXISTS app.workflow_transition_outbox_pending_idx;
CREATE INDEX IF NOT EXISTS workflow_transition_outbox_drainable_idx
  ON app.workflow_transition_outbox (created_at)
  WHERE status IN ('pending', 'processing');
