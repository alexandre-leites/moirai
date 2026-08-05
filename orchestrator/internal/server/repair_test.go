//go:build integration

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
)

// enableRepairLoop flips a seeded project's configuration to opt into the
// bounded repair loop (projectConfig.EnableRepairLoop), read fresh by
// repairEligible on every rejected or unresolved review verdict, so this can
// run any time before the persistExecutionEvent/RecoverOnce call under test.
func (h *harness) enableRepairLoop(projectID string) {
	h.t.Helper()
	h.exec(`UPDATE app.projects SET configuration = configuration || '{"enable_repair_loop":true}'::jsonb WHERE id=$1`, projectID)
}

// ciRepairAttempts reads back app.workflow_runs.ci_repair_attempts, which
// every code path before #354 left permanently at its schema default of 0.
func (h *harness) ciRepairAttempts(workflowID string) int {
	h.t.Helper()
	var attempts int
	if err := h.pool.QueryRow(context.Background(), `SELECT ci_repair_attempts FROM app.workflow_runs WHERE id=$1`, workflowID).Scan(&attempts); err != nil {
		h.t.Fatal(err)
	}
	return attempts
}

// readOffer reads the next message off a runner's outbound channel and
// requires it to be a JobOffer, returning it so a test can inspect its
// TaskPacketJson -- drainOffer's cousin, for a test that needs the payload
// rather than just discarding it.
func (h *harness) readOffer(outbound chan *runnerv1.OrchestratorToRunner) *runnerv1.JobOffer {
	h.t.Helper()
	select {
	case message := <-outbound:
		offer := message.GetOffer()
		if offer == nil {
			h.t.Fatalf("expected a JobOffer, got %T", message.GetMessage())
		}
		return offer
	case <-time.After(time.Second):
		h.t.Fatal("timed out waiting for a job offer")
		return nil
	}
}

// A project that never sets enable_repair_loop must behave exactly as V1
// always has: a rejected review blocks immediately, and ci_repair_attempts
// never moves off its schema default. This is the config-gating guarantee
// the opt-in rests on, mirroring TestAiReviewDisabledByDefaultLeavesDeliveryUnchanged.
func TestRepairLoopDisabledByDefaultBlocksOnRejection(t *testing.T) {
	h := newHarness(t)
	jobID, workflowID, generation := h.runReview(t)
	var runnerID string
	if err := h.pool.QueryRow(context.Background(), `SELECT runner_id::text FROM app.jobs WHERE id=$1`, jobID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"rejected","findings":["missing test coverage"]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked; the repair loop defaults to off", state.status)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != 0 {
		t.Fatalf("ci_repair_attempts = %d, want 0", attempts)
	}
	if role := h.jobRole(jobID); role != "reviewer" {
		t.Fatalf("job role = %q, want reviewer; nothing should have reopened it", role)
	}
}

// The opt-in path: a rejected review on a project with both enable_ai_review
// and enable_repair_loop set must dispatch a bounded repair attempt instead
// of blocking -- reopening the one job back to 'developer', moving the run to
// StatusRepairing, counting the attempt in ci_repair_attempts, and handing
// the reopened execution the reviewer's own findings via the packet's
// reviewFindings field.
func TestRejectedReviewDispatchesABoundedRepairAttempt(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	h.enableRepairLoop(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	outbound := h.outboundChannel(runnerID)
	h.drainOffer(outbound) // the original developer offer

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent (developer completed): %v", err)
	}
	h.drainOffer(outbound) // the reviewer offer
	reviewGeneration, _, err := h.acceptOffer(context.Background(), runnerID, jobID)
	if err != nil {
		t.Fatalf("acceptOffer (reviewer offer): %v", err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: reviewGeneration, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"rejected","findings":["missing input validation on the new endpoint"]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "repairing" {
		t.Fatalf("status = %q, want repairing", state.status)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != 1 {
		t.Fatalf("ci_repair_attempts = %d, want 1", attempts)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer; the repair attempt reopens it", role)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 1 {
		t.Fatal("a run under repair is still active work and must keep its project lock")
	}

	offer := h.readOffer(outbound)
	var packet struct {
		Role           string   `json:"role"`
		ReviewFindings []string `json:"reviewFindings"`
		Constraints    struct {
			MayModifyFiles bool `json:"mayModifyFiles"`
			MayPush        bool `json:"mayPush"`
			MayMerge       bool `json:"mayMerge"`
		} `json:"constraints"`
	}
	if err := json.Unmarshal([]byte(offer.GetTaskPacketJson()), &packet); err != nil {
		t.Fatalf("decode repair packet: %v", err)
	}
	if packet.Role != "developer" {
		t.Fatalf("packet role = %q, want developer", packet.Role)
	}
	if !packet.Constraints.MayModifyFiles || !packet.Constraints.MayPush || packet.Constraints.MayMerge {
		t.Fatalf("repair packet constraints = %+v, want modify+push allowed, merge refused", packet.Constraints)
	}
	if len(packet.ReviewFindings) != 1 || packet.ReviewFindings[0] != "missing input validation on the new endpoint" {
		t.Fatalf("packet reviewFindings = %v, want the rejecting review's own findings", packet.ReviewFindings)
	}
}

// A repaired developer attempt's own "completed" event must flow back through
// the ordinary developer-completion path: with AI review still enabled, that
// means re-entering review rather than delivering unreviewed code, exactly
// the same branch the very first developer attempt took.
func TestRepairedDeveloperCompletionReentersReview(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	h.enableRepairLoop(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	outbound := h.outboundChannel(runnerID)
	h.drainOffer(outbound)

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent (developer completed): %v", err)
	}
	h.drainOffer(outbound)
	reviewGeneration, _, err := h.acceptOffer(context.Background(), runnerID, jobID)
	if err != nil {
		t.Fatalf("acceptOffer (reviewer offer): %v", err)
	}
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: reviewGeneration, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"rejected","findings":["missing input validation"]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}
	h.drainOffer(outbound) // the repair offer
	repairGeneration, _, err := h.acceptOffer(context.Background(), runnerID, jobID)
	if err != nil {
		t.Fatalf("acceptOffer (repair offer): %v", err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: repairGeneration, EventSequence: 3, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent (repaired developer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_ai_review" {
		t.Fatalf("status = %q, want waiting_ai_review; the repaired attempt must be reviewed again", state.status)
	}
	if role := h.jobRole(jobID); role != "reviewer" {
		t.Fatalf("job role = %q, want reviewer; the repaired attempt should have been redispatched for review", role)
	}
	if cycles := h.reviewCycles(workflowID); cycles != 2 {
		t.Fatalf("review_cycles = %d, want 2 (the original review plus the repair's own)", cycles)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != 1 {
		t.Fatalf("ci_repair_attempts = %d, want 1 unchanged; re-entering review does not spend another attempt", attempts)
	}
}

// The bound must actually be enforced: once a run has already spent
// max_repair_attempts, a further rejection must fall through to blocking
// exactly like the opted-out case, not repair forever.
func TestRepairBoundEnforcedFallsThroughToBlocking(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	h.enableRepairLoop(projectID)
	jobID, workflowID, generation := h.runReview(t)
	h.exec(`UPDATE app.workflow_runs SET ci_repair_attempts=$1 WHERE id=$2`, maxRepairAttempts, workflowID)
	var runnerID string
	if err := h.pool.QueryRow(context.Background(), `SELECT runner_id::text FROM app.jobs WHERE id=$1`, jobID).Scan(&runnerID); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "completed",
		PayloadJson: `{"result":{"verdict":"rejected","findings":["still broken"]}}`,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (reviewer completed): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked; the repair bound was already spent", state.status)
	}
	if !strings.Contains(state.blocking, "still broken") {
		t.Fatalf("blocking_reason = %q, want it to carry the reviewer's findings", state.blocking)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != int(maxRepairAttempts) {
		t.Fatalf("ci_repair_attempts = %d, want unchanged at %d; a blocked run must not spend an attempt it never used", attempts, maxRepairAttempts)
	}
	if role := h.jobRole(jobID); role != "reviewer" {
		t.Fatalf("job role = %q, want reviewer; nothing should have reopened it past the bound", role)
	}
}

// If the process dies between persistExecutionEvent committing a rejecting
// reviewer's terminal event and handleReviewCompletion's own follow-on
// decision, the recovery sweep must apply the same repair decision the
// inline path would have, not just the plain block outcome
// TestRecoverySweepAppliesAStrandedReviewVerdict already covers for an
// approval.
func TestRecoverySweepAppliesRepairForAStrandedRejectedVerdict(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableAiReview(projectID)
	h.enableRepairLoop(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	h.exec(`UPDATE app.jobs SET role='reviewer', status='completed', finished_at=now() WHERE id=$1`, jobID)
	h.exec(`INSERT INTO app.ai_reviews(id, workflow_run_id, commit_sha, verdict, result) VALUES ($1,$2,'','rejected','{"findings":["stale cache not invalidated"]}'::jsonb)`, idgen.NewID(), workflowID)
	h.exec(`UPDATE app.workflow_runs SET status='waiting_ai_review', current_phase='waiting_ai_review', updated_at=now()-interval '10 minutes' WHERE id=$1`, workflowID)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "repairing" {
		t.Fatalf("status = %q, want repairing; the stranded rejection should have been repaired", state.status)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != 1 {
		t.Fatalf("ci_repair_attempts = %d, want 1", attempts)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer", role)
	}
}
