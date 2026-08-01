-- Issue state was stored exactly as the code host spelled it. The GitHub CLI
-- reports "OPEN", while every query in persistence/control_plane.py compares
-- against 'open' -- so a synchronised issue was invisible to the queue, counted
-- as neither open nor eligible in the sync status, and excluded from the
-- queue-depth metric. The console showed "0/0" for a project whose issues had
-- just synchronised successfully.
--
-- upsert_issue now lowercases on write. This normalises what is already stored,
-- so an existing deployment does not have to wait for the next synchronisation
-- of every issue to become consistent.
UPDATE app.issues
SET state = lower(state)
WHERE state <> lower(state);

-- Keeps the column canonical regardless of which caller writes it next: the
-- application normalises, and this makes a caller that forgets an error rather
-- than a project whose issues silently stop being scheduled.
ALTER TABLE app.issues
  DROP CONSTRAINT IF EXISTS issues_state_is_lowercase;
ALTER TABLE app.issues
  ADD CONSTRAINT issues_state_is_lowercase CHECK (state = lower(state));
