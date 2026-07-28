-- Let a runner declare how many executions it can work on concurrently
-- (for different projects; ProjectConcurrencyGuard still serializes a
-- single project's shared worktree on the runner side).
ALTER TABLE app.runners ADD COLUMN IF NOT EXISTS capacity INTEGER NOT NULL DEFAULT 1;
ALTER TABLE app.runners ADD CONSTRAINT runners_capacity_positive CHECK (capacity > 0);
