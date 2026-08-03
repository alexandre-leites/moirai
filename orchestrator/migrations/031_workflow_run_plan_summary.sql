-- Records what the planning phase (#351) produced, so it survives past the
-- single request that dispatched the developer packet it feeds and stays
-- visible to an operator (console/API) for the life of the run. NULL for
-- every workflow run created before this column existed, and for any run
-- whose project never opted into RequirePlanning.
ALTER TABLE app.workflow_runs ADD COLUMN IF NOT EXISTS plan_summary TEXT;
