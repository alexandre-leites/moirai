-- Indexes for the counts the Prometheus surface reads on every scrape.
--
-- The same reasoning as 019: `GetSchedulerMetrics`'s query used to run only
-- when an operator opened the console, and now runs on Prometheus's scrape
-- interval as well, against two tables that only grow. Neither of its two
-- status predicates could use an existing index -- the only full index on
-- app.jobs leads with runner_id, and nothing on app.workflow_runs covers a
-- status the partial indexes exclude -- so both counted by scanning the whole
-- history to measure the in-flight set.
--
-- Both are partial, so each stays the size of the in-flight set rather than of
-- the history, and neither is written by a row that has reached a terminal
-- status. The column is the status itself: the index has to answer a count, so
-- an index-only scan over the matching rows is the whole of the work.

CREATE INDEX IF NOT EXISTS jobs_in_flight_idx ON app.jobs (status) WHERE status IN ('offered', 'preparing', 'running');

CREATE INDEX IF NOT EXISTS workflow_runs_active_idx ON app.workflow_runs (status) WHERE status NOT IN ('completed', 'blocked', 'failed', 'cancelled');
