//go:build integration

package server

import (
	"context"
	"strings"
	"testing"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
)

// jobRole reads back app.jobs.role for jobID, the column dispatchReviewerJob
// flips from 'developer' to 'reviewer' when it reopens a workflow run's one
// job for an independent review.
func (h *harness) jobRole(jobID string) string {
	h.t.Helper()
	var role string
	if err := h.pool.QueryRow(context.Background(), `SELECT role FROM app.jobs WHERE id=$1`, jobID).Scan(&role); err != nil {
		h.t.Fatal(err)
	}
	return role
}

// reviewCycles reads back app.workflow_runs.review_cycles, which every code
// path before #353 left permanently at its schema default of 0.
func (h *harness) reviewCycles(workflowID string) int {
	h.t.Helper()
	var cycles int
	if err := h.pool.QueryRow(context.Background(), `SELECT review_cycles FROM app.workflow_runs WHERE id=$1`, workflowID).Scan(&cycles); err != nil {
		h.t.Fatal(err)
	}
	return cycles
}

// A project that never sets enable_ai_review must behave exactly as it did
// before #353: a developer's "completed" event goes straight to delivery, no
// reviewer job is ever dispatched, and review_cycles never moves off its
// schema default. This is the config-gating guarantee the opt-in rests on.
func TestAiReviewDisabledByDefaultLeavesDeliveryUnchanged(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	if state := h.runState(workflowID); state.status != "waiting_github_checks" {
		t.Fatalf("status = %q, want waiting_github_checks (AI review disabled must not change delivery)", state.status)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer; nothing should have reopened this job", role)
	}
	if cycles := h.reviewCycles(workflowID); cycles != 0 {
		t.Fatalf("review_cycles = %d, want 0", cycles)
	}
	if reviews := h.scalar(`SELECT COUNT(*) FROM app.ai_reviews WHERE workflow_run_id=$1`, workflowID); reviews != 0 {
		t.Fatalf("app.ai_reviews rows = %d, want 0", reviews)
	}
}

// The opt-in path: a developer's "completed" event on a project with
// enable_ai_review set must park the run at StatusWaitingAiReview, reopen its
// one job for an independent reviewer execution instead of delivering, and
// count the attempt in review_cycles -- for real, not the permanent zero
// every path left it at before #353.
func TestDeveloperCompletionDispatchesAnIndependentReviewerJob(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_ai_review" {
		t.Fatalf("status = %q, want waiting_ai_review", state.status)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 1 {
		t.Fatal("a run awaiting AI review must keep its project lock; it is still active work")
	}
	if role := h.jobRole(jobID); role != "reviewer" {
		t.Fatalf("job role = %q, want reviewer; the one job should have been reopened", role)
	}
	if jobStatus := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE id=$1 AND status='offered'`, jobID); jobStatus != 1 {
		t.Fatal("the reopened job was not offered to a runner")
	}
	if offers := h.scalar(`SELECT COUNT(*) FROM app.job_offers WHERE job_id=$1 AND status='offered'`, jobID); offers != 1 {
		t.Fatalf("job_offers rows for the reopened job = %d, want 1", offers)
	}
	if cycles := h.reviewCycles(workflowID); cycles != 1 {
		t.Fatalf("review_cycles = %d, want 1", cycles)
	}
	if prs := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1`, workflowID); prs != 0 {
		t.Fatal("a run awaiting AI review must not have a pull request opened yet")
	}
}

// runReview drives a run all the way to a completed, reopened reviewer
// execution: h.project() (opted into AI review), a developer job run to
// completion, and the resulting reviewer offer accepted by the same runner.
// Returns the job/workflow IDs and the reviewer execution's own lease
// generation, ready for the caller to report the reviewer's own terminal
// event against.
func (h *harness) runReview(t *testing.T) (jobID, workflowID string, reviewGeneration int64) {
	t.Helper()
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	runnerID := h.runner()
	jobID, workflowID = h.runJob(runnerID)
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent (developer completed): %v", err)
	}
	generation, _, err := h.acceptOffer(context.Background(), runnerID, jobID)
	if err != nil {
		t.Fatalf("acceptOffer (reviewer offer): %v", err)
	}
	if state := h.runState(workflowID); state.status != "waiting_ai_review" {
		t.Fatalf("status after accepting the reviewer offer = %q, want waiting_ai_review unchanged", state.status)
	}
	return jobID, workflowID, generation
}

// An approving verdict is the same success path a project with AI review
// disabled always used: deliverWorkflow runs, and the verdict is recorded
// rather than discarded.
func TestApprovingReviewVerdictDeliversThePullRequest(t *testing.T) {
	h := newHarness(t)
	jobID, workflowID, generation := h.runReview(t)
	var runnerID string
	if err := h.pool.QueryRow(context.Background(), `SELECT runner_id::text FROM app.jobs WHERE id=$1`, jobID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"approved","findings":[]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_github_checks" {
		t.Fatalf("status = %q, want waiting_github_checks; an approving review must deliver", state.status)
	}
	if prs := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1`, workflowID); prs != 1 {
		t.Fatalf("pull_requests rows = %d, want 1", prs)
	}
	if reviews := h.scalar(`SELECT COUNT(*) FROM app.ai_reviews WHERE workflow_run_id=$1 AND verdict='approved'`, workflowID); reviews != 1 {
		t.Fatal("the approving verdict was not recorded in app.ai_reviews")
	}
}

// A rejecting verdict must not silently deliver: it ends the run blocked, with
// the reviewer's findings as the reason -- the same terminal shape a failed
// deterministic pipeline check or an agent's own declared block already use,
// and the signal #354's repair loop is meant to consume.
func TestRejectingReviewVerdictBlocksWithFindings(t *testing.T) {
	h := newHarness(t)
	jobID, workflowID, generation := h.runReview(t)
	var runnerID string
	if err := h.pool.QueryRow(context.Background(), `SELECT runner_id::text FROM app.jobs WHERE id=$1`, jobID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"rejected","findings":["missing test coverage for the new branch"]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked", state.status)
	}
	if !strings.Contains(state.blocking, "missing test coverage for the new branch") {
		t.Fatalf("blocking_reason = %q, want it to carry the reviewer's findings", state.blocking)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a rejected review must release the project lock like any other terminal block")
	}
	if reviews := h.scalar(`SELECT COUNT(*) FROM app.ai_reviews WHERE workflow_run_id=$1 AND verdict='rejected'`, workflowID); reviews != 1 {
		t.Fatal("the rejecting verdict was not recorded in app.ai_reviews")
	}
}

// A reviewer execution that crashes or is cancelled without ever producing a
// verdict must not be mistaken for an ordinary developer failure
// (agentBlockReason's shape), nor silently deliver: it blocks, the same
// terminal outcome a rejecting verdict reaches.
func TestReviewerExecutionFailureBlocksTheRun(t *testing.T) {
	h := newHarness(t)
	jobID, workflowID, generation := h.runReview(t)
	var runnerID string
	if err := h.pool.QueryRow(context.Background(), `SELECT runner_id::text FROM app.jobs WHERE id=$1`, jobID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "failed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer failed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked", state.status)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a crashed reviewer execution must release the project lock")
	}
}

// If no runner was connected at the moment a developer execution completed,
// dispatchReviewerJob's inline attempt has nothing to offer to. The run must
// not be stranded forever at waiting_ai_review: once old enough, the recovery
// sweep retries the dispatch against whichever runner is connected by then.
func TestRecoverySweepRedispatchesAStrandedReviewWhenNoRunnerWasConnected(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	// Simulate the runner dropping its control stream right before the
	// developer's own completion event arrives: dispatchReviewerJob's inline
	// call inside persistExecutionEvent sees no connected runner and does
	// nothing further. removeSession only clears a session that still
	// matches its own channel value, so it is looked up and cleared directly.
	h.mu.Lock()
	delete(h.sessions, runnerID)
	h.mu.Unlock()

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}
	if state := h.runState(workflowID); state.status != "waiting_ai_review" {
		t.Fatalf("status = %q, want waiting_ai_review", state.status)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer; nothing should have dispatched yet", role)
	}

	// Not stale enough yet: the sweep must leave an in-flight dispatch alone.
	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatal("the recovery sweep redispatched a review before it was old enough to call stranded")
	}

	h.exec(`UPDATE app.workflow_runs SET updated_at=now()-interval '10 minutes' WHERE id=$1`, workflowID)
	if !h.addSession(runnerID, make(chan *runnerv1.OrchestratorToRunner, 16)) {
		t.Fatal("runner session was already registered")
	}

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	if role := h.jobRole(jobID); role != "reviewer" {
		t.Fatalf("job role = %q, want reviewer; the recovery sweep should have redispatched it", role)
	}
	if cycles := h.reviewCycles(workflowID); cycles != 1 {
		t.Fatalf("review_cycles = %d, want 1", cycles)
	}
}

// If the process dies between persistExecutionEvent committing a reviewer's
// terminal event and handleReviewCompletion's own follow-on delivery/block
// decision, the run is stuck at waiting_ai_review with a completed reviewer
// job and an app.ai_reviews row already on file. The recovery sweep must
// re-derive the same decision from that row instead of waiting forever, and
// must not insert a second app.ai_reviews row for the same execution.
func TestRecoverySweepAppliesAStrandedReviewVerdict(t *testing.T) {
	h := newHarness(t)
	jobID, workflowID, _ := h.runReview(t)

	// Simulate persistExecutionEvent's commit having already landed (the job
	// is the completed reviewer execution) without handleReviewCompletion's
	// own follow-on ever running.
	h.exec(`UPDATE app.jobs SET status='completed', finished_at=now() WHERE id=$1`, jobID)
	h.exec(`INSERT INTO app.ai_reviews(id, workflow_run_id, commit_sha, verdict, result) VALUES ($1,$2,'','approved','{}'::jsonb)`, idgen.NewID(), workflowID)
	h.exec(`UPDATE app.workflow_runs SET updated_at=now()-interval '10 minutes' WHERE id=$1`, workflowID)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_github_checks" {
		t.Fatalf("status = %q, want waiting_github_checks; the recorded approval should have delivered", state.status)
	}
	if reviews := h.scalar(`SELECT COUNT(*) FROM app.ai_reviews WHERE workflow_run_id=$1`, workflowID); reviews != 1 {
		t.Fatalf("app.ai_reviews rows = %d, want 1; the recovery sweep must not insert a second one", reviews)
	}
}
