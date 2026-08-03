-- name: GetWorkflowRepairState :one
-- repairEligible's (repair.go) single read: whether the project opted into
-- EnableRepairLoop (projectConfig) and how many repair attempts this run has
-- already spent, so the decision to repair or block can be made without
-- assuming dispatchRepairJob's own guarded query below will ever run --
-- EnableRepairLoop off, or the bound already reached, must block immediately
-- rather than silently doing nothing.
SELECT p.configuration, wr.ci_repair_attempts
FROM app.workflow_runs wr
JOIN app.projects p ON p.id = wr.project_id
WHERE wr.id = sqlc.arg(id);

-- name: GetRepairDispatchWorkflow :one
-- Everything dispatchRepairJob (repair.go) needs to build and offer a repair
-- developer execution against the one job a workflow run already has,
-- informed by the most recent independent review's own findings. Guarded the
-- same way GetReviewDispatchWorkflow is: only a run sitting at
-- 'waiting_ai_review' whose job is still the completed reviewer job is ever
-- repairable, and max_repair_attempts is enforced here too -- belt and
-- suspenders alongside repairEligible's own check, the same redundancy
-- GetReviewDispatchWorkflow accepts for its own guard.
SELECT wr.project_id::text AS project_id, i.external_id, i.title, i.body,
       p.repository_mode, COALESCE(p.repository_url, '') AS repository_url,
       COALESCE(p.local_repository_path, '') AS local_repository_path,
       p.default_branch, COALESCE(wr.branch_name, '') AS branch_name,
       p.configuration, wr.ci_repair_attempts, j.id::text AS job_id,
       (COALESCE(
         (SELECT ar.result FROM app.ai_reviews ar WHERE ar.workflow_run_id = wr.id ORDER BY ar.created_at DESC LIMIT 1),
         '{}'::jsonb
       ))::jsonb AS latest_review_result
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
JOIN app.jobs j ON j.workflow_run_id = wr.id
WHERE wr.id = sqlc.arg(id) AND wr.status = 'waiting_ai_review' AND j.role = 'reviewer' AND j.status = 'completed'
  AND wr.ci_repair_attempts < sqlc.arg(max_repair_attempts)::integer;

-- name: MarkWorkflowRepairing :execrows
-- Increments ci_repair_attempts and moves the run to 'repairing' in one
-- guarded statement, so a caller that raced this one (the recovery sweep
-- calling applyRecordedReviewVerdict against the same run dispatchRepairJob's
-- inline call already claimed) affects 0 rows instead of double-spending an
-- attempt.
UPDATE app.workflow_runs
SET status = 'repairing', current_phase = 'repairing', ci_repair_attempts = ci_repair_attempts + 1, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'waiting_ai_review' AND ci_repair_attempts < sqlc.arg(max_repair_attempts)::integer;

-- name: ReopenJobForRepair :one
-- Reuses the workflow run's single job row for a third role -- back to
-- 'developer', but this time for a repair attempt rather than the original
-- implementation -- instead of inserting a new one (app.jobs.workflow_run_id
-- stays UNIQUE), the same reuse ReopenJobForReview already established for
-- the reviewer role. Guarded on the job still being the completed reviewer
-- job whose rejection triggered this repair; a second caller that raced this
-- one finds 0 rows affected and does nothing further.
UPDATE app.jobs
SET role = 'developer', runner_id = sqlc.arg(runner_id)::uuid, status = 'offered',
    offered_at = now(), accepted_at = NULL, started_at = NULL, finished_at = NULL,
    last_event_sequence = 0, lease_generation = lease_generation + 1, recovery_reason = NULL
WHERE id = sqlc.arg(id) AND role = 'reviewer' AND status = 'completed'
RETURNING lease_generation;
