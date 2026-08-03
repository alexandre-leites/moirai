-- Adds the 'waiting_ai_review' status (#353): the independent-AI-review gate
-- between an implementation's own reported success and delivery. A run parked
-- here is still active work -- see internal/server/status.go's
-- terminalStatuses/genuinelyTerminalStatuses, which deliberately exclude it
-- the same way 025_workflow_run_waiting_human_status.sql's 'waiting_human'
-- was excluded before it. A CHECK constraint cannot be altered in place; it
-- has to be dropped and recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'waiting_github_checks', 'waiting_human', 'waiting_ai_review', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );

-- app.jobs gains a role so the one job a workflow run ever has (its
-- workflow_run_id stays UNIQUE, unchanged by this migration) can be reopened
-- for a second, independent execution -- an AI reviewer -- instead of always
-- meaning the original developer execution. Every job that already exists (and
-- every one CreateJob inserts, unchanged, for the scheduler's own developer
-- dispatch) is a developer job by definition, hence the default.
ALTER TABLE app.jobs ADD COLUMN role TEXT NOT NULL DEFAULT 'developer' CHECK (role IN ('developer', 'reviewer'));
