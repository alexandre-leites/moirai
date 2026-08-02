-- Indexes for the queries the Go orchestrator runs on a timer.
--
-- Each of these backs a predicate that previously had no usable index and now
-- runs on a schedule rather than only when an operator asked for it, so the
-- cost is paid continuously against tables that only grow.

-- The issue-sync upsert asks "has this issue ever had a workflow run?" once per
-- issue, every sync pass. workflow_runs.issue_id is a foreign key, and Postgres
-- does not index referencing columns automatically, so each check was a full
-- scan — and the common answer, "no run exists", is the case that has to read
-- the whole table to prove it.
CREATE INDEX IF NOT EXISTS workflow_runs_issue_idx ON app.workflow_runs (issue_id);

-- The recovery sweep looks for leases nobody is renewing every 30 seconds. The
-- only existing index on app.jobs leads with runner_id, which this predicate
-- cannot use. Partial, so it stays the size of the in-flight set rather than of
-- the job history.
CREATE INDEX IF NOT EXISTS jobs_expired_lease_idx ON app.jobs (lease_expires_at) WHERE status IN ('preparing', 'running');

-- The same sweep reclaims offers that were never answered.
CREATE INDEX IF NOT EXISTS jobs_unanswered_offer_idx ON app.jobs (offered_at) WHERE status = 'offered';

-- And re-drives deliveries interrupted before the pull request was opened.
CREATE INDEX IF NOT EXISTS workflow_runs_stranded_delivery_idx ON app.workflow_runs (updated_at, id) WHERE status = 'completed';
