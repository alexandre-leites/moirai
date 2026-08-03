-- Adds the 'repairing' status (#354): a bounded, AI-review-informed repair
-- attempt dispatched instead of blocking a run on the first AI-review
-- rejection. A run parked here is still active work -- see
-- internal/server/status.go's terminalStatuses/genuinelyTerminalStatuses,
-- which deliberately exclude it the same way 028_workflow_run_ai_review.sql's
-- 'waiting_ai_review' was excluded before it. A CHECK constraint cannot be
-- altered in place; it has to be dropped and recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'waiting_github_checks', 'waiting_human', 'waiting_ai_review', 'repairing', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );
