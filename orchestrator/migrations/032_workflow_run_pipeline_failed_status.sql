-- Adds the 'pipeline_failed' status (#352): a developer (or repair) execution
-- whose agent succeeded but whose project's own deterministic local pipeline
-- -- PROJECT.md's "completion gate" -- failed a required command. A run
-- parked here is still active work: persistExecutionEvent (server.go) sets it
-- in place of 'delivering' the moment the pipeline reports a required
-- failure, and it holds the project lock exactly like 'waiting_ai_review'
-- does while pipelineFailedOrBlock (repair.go) decides whether to repair it
-- (bounded by the same EnableRepairLoop opt-in and maxRepairAttempts bound
-- #354 already established, spent from its own pipeline_repair_attempts
-- column instead of ci_repair_attempts) or end the run at 'blocked'. See
-- internal/server/status.go's terminalStatuses/genuinelyTerminalStatuses,
-- which deliberately exclude it the same way 'waiting_ai_review' and
-- 'repairing' are.
--
-- A CHECK constraint cannot be altered in place; it has to be dropped and
-- recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'planning', 'waiting_github_checks', 'waiting_human', 'waiting_ai_review', 'pipeline_failed', 'repairing', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );
