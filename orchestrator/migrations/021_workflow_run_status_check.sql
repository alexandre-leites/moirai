-- app.workflow_runs.status is the central state of the control plane, and
-- until now was the one status column in the schema without a CHECK: every
-- other one (004, 005, 016's own case for why) constrains its vocabulary.
-- Without it, the only thing keeping the column's values consistent with
-- everything that reads them (server.go's terminalStatuses, the
-- workflow_runs_active_idx partial index from 020, web/src/status.ts) was
-- every writer agreeing by hand. #265 gives the vocabulary a Go type
-- (internal/server/status.go); this migration is the database's half of that.
--
-- The seven values below are every one server.go, delivery.go and recovery.go
-- write to this column today: 'offered' and 'preparing' while a run is being
-- scheduled, 'waiting_github_checks' while its pull request waits on CI, and
-- the four terminalStatuses ('completed', 'failed', 'blocked', 'cancelled').
-- 'running' is deliberately absent -- it is a status app.jobs uses, but no
-- code path ever writes it to a workflow run.
ALTER TABLE app.workflow_runs
  ADD CONSTRAINT workflow_runs_status_is_known CHECK (
    status IN ('offered', 'preparing', 'waiting_github_checks', 'completed', 'failed', 'blocked', 'cancelled')
  );
