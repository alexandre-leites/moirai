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

-- name: GetPipelineRepairState :one
-- pipelineRepairEligible's (repair.go) single read: whether the project
-- opted into EnableRepairLoop (projectConfig, the same toggle #354's own
-- ci_repair_attempts track uses) and how many pipeline-triggered repair
-- attempts this run has already spent, so the decision to repair or block a
-- failed deterministic pipeline can be made without assuming
-- dispatchPipelineRepairJob's own guarded query below will ever run.
SELECT p.configuration, wr.pipeline_repair_attempts
FROM app.workflow_runs wr
JOIN app.projects p ON p.id = wr.project_id
WHERE wr.id = sqlc.arg(id);

-- name: GetPipelineRepairDispatchWorkflow :one
-- Everything dispatchPipelineRepairJob (repair.go) needs to build and offer a
-- repair developer execution against the one job a workflow run already has,
-- informed by the failing command persistExecutionEvent recorded as
-- blocking_reason. Guarded the same shape GetRepairDispatchWorkflow is: only
-- a run sitting at 'pipeline_failed' whose job is still the failed developer
-- job is ever repairable this way, and max_repair_attempts is enforced here
-- too -- belt and suspenders alongside pipelineRepairEligible's own check.
SELECT wr.project_id::text AS project_id, i.external_id, i.title, i.body,
       p.repository_mode, COALESCE(p.repository_url, '') AS repository_url,
       COALESCE(p.local_repository_path, '') AS local_repository_path,
       p.default_branch, COALESCE(wr.branch_name, '') AS branch_name,
       p.configuration, wr.pipeline_repair_attempts, j.id::text AS job_id,
       COALESCE(wr.blocking_reason, '') AS blocking_reason
FROM app.workflow_runs wr
JOIN app.issues i ON i.id = wr.issue_id
JOIN app.projects p ON p.id = wr.project_id
JOIN app.jobs j ON j.workflow_run_id = wr.id
WHERE wr.id = sqlc.arg(id) AND wr.status = 'pipeline_failed' AND j.role = 'developer' AND j.status = 'failed'
  AND wr.pipeline_repair_attempts < sqlc.arg(max_repair_attempts)::integer;

-- name: MarkWorkflowPipelineRepairing :execrows
-- Increments pipeline_repair_attempts and moves the run to 'repairing' in one
-- guarded statement, so a caller that raced this one (the recovery sweep
-- calling applyStrandedPipelineDecision against the same run
-- dispatchPipelineRepairJob's inline call already claimed) affects 0 rows
-- instead of double-spending an attempt.
UPDATE app.workflow_runs
SET status = 'repairing', current_phase = 'repairing', pipeline_repair_attempts = pipeline_repair_attempts + 1, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'pipeline_failed' AND pipeline_repair_attempts < sqlc.arg(max_repair_attempts)::integer;

-- name: ReopenJobForPipelineRepair :one
-- Reuses the workflow run's single job row for another developer execution --
-- role stays 'developer' throughout, unlike ReopenJobForRepair's
-- reviewer-to-developer transition, since a pipeline failure never routed the
-- job through a reviewer role in the first place. Guarded on the job still
-- being the failed developer job whose pipeline failure triggered this
-- repair; a second caller that raced this one finds 0 rows affected and does
-- nothing further.
UPDATE app.jobs
SET role = 'developer', runner_id = sqlc.arg(runner_id)::uuid, status = 'offered',
    offered_at = now(), accepted_at = NULL, started_at = NULL, finished_at = NULL,
    last_event_sequence = 0, lease_generation = lease_generation + 1, recovery_reason = NULL
WHERE id = sqlc.arg(id) AND role = 'developer' AND status = 'failed'
RETURNING lease_generation;

-- name: GetWorkflowBlockingReason :one
-- applyStrandedPipelineDecision's (recovery.go) read of the reason
-- persistExecutionEvent already stored in blocking_reason when it set the run
-- to 'pipeline_failed', so the sweep can re-apply pipelineFailedOrBlock
-- without re-parsing the original runner payload (not re-fetched here).
SELECT COALESCE(blocking_reason, '') FROM app.workflow_runs WHERE id = sqlc.arg(id);

-- name: SelectStrandedPipelineFailureWorkflows :many
-- resumeStrandedPipelineDecisions' (recovery.go) candidate set: a run still
-- sitting at 'pipeline_failed' -- persistExecutionEvent committed the
-- terminal event, then the process died (or found no connected/eligible
-- runner) before pipelineFailedOrBlock's own repair-or-block decision landed.
-- Unlike a rejected AI review, a failed pipeline needs no second execution to
-- produce a verdict -- it already has one -- so this sweep exists only to
-- retry the decision itself once a runner is available, not to wait for one.
SELECT wr.id::text AS id
FROM app.workflow_runs wr
WHERE wr.status = 'pipeline_failed'
  AND wr.updated_at < now() - sqlc.arg(stranded_pipeline_failure)::interval
ORDER BY wr.updated_at, wr.id
LIMIT 20;
