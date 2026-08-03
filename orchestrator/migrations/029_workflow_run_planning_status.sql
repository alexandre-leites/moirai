-- Adds 'planning' to the vocabulary 021_workflow_run_status_check.sql
-- enforces (022 added 'delivering', 025 added 'waiting_human', 028 added
-- 'waiting_ai_review'); internal/server/status.go's StatusPlanning, added by
-- #351.
--
-- Before this status existed, ScheduleOnce dispatched a workflow's single
-- developer execution directly -- PROJECT.md's planning phase (see #250) was
-- never wired up, and workflow_runs.planning_attempts stayed 0 forever.
-- 'planning' gives a project that opts into RequirePlanning (projectConfig)
-- somewhere to sit while its planner packet runs: it holds the project lock
-- exactly like 'preparing' does for the developer execution that follows it
-- (see status.go's doc comment for why it is deliberately absent from
-- terminalStatuses/genuinelyTerminalStatuses), and is excluded here for the
-- same reason.
--
-- A CHECK constraint cannot be altered in place; it has to be dropped and
-- recreated with the wider set.
ALTER TABLE app.workflow_runs DROP CONSTRAINT IF EXISTS workflow_runs_status_is_known;

ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'planning', 'waiting_github_checks', 'waiting_human', 'waiting_ai_review', 'delivering', 'completed', 'failed', 'blocked', 'cancelled')
  );

-- app.jobs.role (028_workflow_run_ai_review.sql, #353) already lets the one
-- job a workflow run ever has be reopened for a second, independent
-- execution under a role distinct from the original developer one --
-- 'reviewer'. Planning is a *first* execution rather than a second one, but
-- the same one-job-many-roles shape applies: CreateJob now inserts a
-- 'planner' job directly for an opted-in project, and dispatchImplementationJob
-- (server.go) reopens it back to 'developer' once the planner execution
-- completes, mirroring ReopenJobForReview (review.sql) exactly.
ALTER TABLE app.jobs DROP CONSTRAINT jobs_role_check;
ALTER TABLE app.jobs ADD CONSTRAINT jobs_role_check CHECK (role IN ('developer', 'reviewer', 'planner'));
