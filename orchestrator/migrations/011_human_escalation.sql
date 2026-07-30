ALTER TABLE app.workflow_runs
  ADD COLUMN IF NOT EXISTS human_question TEXT,
  ADD COLUMN IF NOT EXISTS human_resume_phase TEXT,
  ADD COLUMN IF NOT EXISTS human_guidance TEXT;
