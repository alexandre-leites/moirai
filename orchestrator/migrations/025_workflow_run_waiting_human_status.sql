-- Adds 'waiting_human' to the vocabulary 021_workflow_run_status_check.sql
-- enforces (022 already added 'delivering'; internal/server/status.go's
-- StatusWaitingHuman, added by #250).
--
-- Before this status existed, observeWorkflow merged a pull request the
-- instant GitHub reported its checks green, with no gate for a project that
-- wants a person to look first: SubmitHumanDecision, the RPC the console's
-- decision panel has called since it was built, always answered
-- "V1 has no approval phase". 'waiting_human' gives observeWorkflow somewhere
-- to stop for an opted-in project (projectConfig.RequireHumanApproval), and
-- SubmitHumanDecision a real status to move a run away from: 'approved' back
-- to 'waiting_github_checks' (the same tested merge path runs again, checks
-- already green), 'changes_requested' to 'blocked' with the reviewer's
-- comment as the reason.
--
-- A CHECK constraint cannot be altered in place; it has to be dropped and
-- recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'waiting_github_checks', 'waiting_human', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );
