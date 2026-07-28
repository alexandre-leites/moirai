-- Let a runner declare how many executions it can work on concurrently
-- (for different projects, ProjectConcurrencyGuard still serializes a
-- single project's shared worktree on the runner side).
ALTER TABLE app.runners ADD COLUMN IF NOT EXISTS capacity INTEGER NOT NULL DEFAULT 1;
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'runners_capacity_positive'
      AND conrelid = 'app.runners'::regclass
  ) THEN
    ALTER TABLE app.runners ADD CONSTRAINT runners_capacity_positive CHECK (capacity > 0);
  END IF;
END $$;
