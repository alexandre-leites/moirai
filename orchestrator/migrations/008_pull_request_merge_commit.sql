-- The merge commit a pull request landed as.
--
-- `app.pull_requests` already had `merged_at` and nothing ever wrote it: the
-- merge node fired `gh pr merge` and transitioned straight on, so the durable
-- record could not tell an attempted merge from a completed one (issue #121).
-- Verifying the merge means re-reading the pull request afterwards, and what
-- comes back is a timestamp *and* the commit the merge produced -- the commit
-- being the part that identifies what actually landed on the default branch.
--
-- Nullable with no default and no backfill: every pull request that predates
-- this column merged (or did not) without anyone recording which commit it
-- became, and inventing a value would be worse than an honest NULL. Rows are
-- filled in by the merge node's confirming read from here on.
--
-- Idempotent, and safe to re-run: ADD COLUMN IF NOT EXISTS is a no-op once the
-- column is there, and the statement takes no table rewrite because the column
-- has no default.
ALTER TABLE app.pull_requests ADD COLUMN IF NOT EXISTS merge_commit TEXT;

-- `state` is now normalised to lower case at the code-host adapter boundary,
-- because a column that spells one state two ways is a column nothing can
-- query. Rows written before that change hold GitHub's own `OPEN`/`CLOSED`/
-- `MERGED`, and leaving them would produce exactly the split this normalisation
-- exists to prevent. The predicate makes the statement a no-op on a re-run and
-- on any database that never held mixed-case rows.
UPDATE app.pull_requests SET state = lower(state) WHERE state <> lower(state);
