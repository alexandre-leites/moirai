-- Adds 'delivering' to the vocabulary 021_workflow_run_status_check.sql
-- enforces (internal/server/status.go's StatusDelivering, added by #267).
--
-- Before this status existed, persistExecutionEvent wrote 'completed' the
-- moment the runner's execution succeeded -- deliberately keeping the
-- project lock -- and observeWorkflow wrote the very same value again once
-- GitHub confirmed the pull request merged. The two meanings, "the runner
-- finished" and "the pull request merged", were distinguishable only by
-- whether a project_locks row still happened to exist, which is why
-- resumeStrandedDeliveries (recovery.go) needed both a lock join and a
-- five-minute age bound to avoid racing an in-progress delivery. 'delivering'
-- gives the first meaning its own value, so a plain
-- `status='delivering' AND updated_at < ...` now finds exactly the same
-- stranded runs the lock join used to.
--
-- A CHECK constraint cannot be altered in place; it has to be dropped and
-- recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'waiting_github_checks', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );

-- 019_recovery_indexes.sql's workflow_runs_stranded_delivery_idx backed the
-- old `status = 'completed'` predicate resumeStrandedDeliveries used; the
-- predicate now reads 'delivering', so the partial index has to move with
-- it or the sweep goes back to scanning the whole table.
DROP INDEX IF EXISTS app.workflow_runs_stranded_delivery_idx;

CREATE INDEX IF NOT EXISTS workflow_runs_stranded_delivery_idx ON app.workflow_runs (updated_at, id) WHERE status = 'delivering';
