//go:build integration

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
)

// addPipelineStep seeds one app.project_pipeline_steps row for projectID --
// the storage ProjectConfiguration.pipeline_steps already writes through
// replacePipelineSteps (server.go), reproduced here at the SQL level so a test
// need not go through the CreateProject/UpdateProject RPCs just to configure
// one command.
func (h *harness) addPipelineStep(projectID, command string, timeoutSeconds int32, required bool) {
	h.t.Helper()
	var position int32
	if err := h.pool.QueryRow(context.Background(), `SELECT COALESCE(MAX(position)+1,0) FROM app.project_pipeline_steps WHERE project_id=$1`, projectID).Scan(&position); err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO app.project_pipeline_steps(id,project_id,position,name,command,timeout_seconds,required) VALUES($1,$2,$3,$4,$4,$5,$6)`,
		idgen.NewID(), projectID, position, command, timeoutSeconds, required)
}

// pipelineRepairAttempts reads back app.workflow_runs.pipeline_repair_attempts,
// which every code path before #352 left permanently at its schema default of
// 0 -- the same gap ci_repair_attempts had before #354.
func (h *harness) pipelineRepairAttempts(workflowID string) int {
	h.t.Helper()
	var attempts int
	if err := h.pool.QueryRow(context.Background(), `SELECT pipeline_repair_attempts FROM app.workflow_runs WHERE id=$1`, workflowID).Scan(&attempts); err != nil {
		h.t.Fatal(err)
	}
	return attempts
}

// A project that never configures a pipeline_steps row must dispatch exactly
// the packet V1 always has: pipeline: [], since dispatch.go's runner-side
// guard (len(packet.Pipeline) > 0) never runs anything for it. This is the
// no-config-change-required-existing-behaviour guarantee the whole feature
// rests on.
func TestDeveloperPacketPipelineEmptyWhenUnconfigured(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	h.runJob(runnerID)
	offer := h.receiveOffer(outbound)

	var packet struct {
		Pipeline []map[string]any `json:"pipeline"`
	}
	if err := json.Unmarshal([]byte(offer.GetTaskPacketJson()), &packet); err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len(packet.Pipeline) != 0 {
		t.Fatalf("pipeline = %#v, want empty for a project with no configured steps", packet.Pipeline)
	}
}

// A project with configured pipeline steps must have them travel into the
// developer packet verbatim, required flag included -- PROJECT.md's
// "deterministic completion gate" only gates anything once this is real.
func TestDeveloperPacketIncludesConfiguredPipelineSteps(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.addPipelineStep(projectID, "make test", 300, true)
	h.addPipelineStep(projectID, "make lint", 120, false)
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	h.runJob(runnerID)
	offer := h.receiveOffer(outbound)

	var packet struct {
		Pipeline []struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeoutSeconds"`
			Required       bool   `json:"required"`
		} `json:"pipeline"`
	}
	if err := json.Unmarshal([]byte(offer.GetTaskPacketJson()), &packet); err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len(packet.Pipeline) != 2 {
		t.Fatalf("pipeline = %#v, want 2 configured steps", packet.Pipeline)
	}
	if packet.Pipeline[0].Command != "make test" || packet.Pipeline[0].TimeoutSeconds != 300 || !packet.Pipeline[0].Required {
		t.Fatalf("pipeline[0] = %#v, want the required make test step", packet.Pipeline[0])
	}
	if packet.Pipeline[1].Command != "make lint" || packet.Pipeline[1].Required {
		t.Fatalf("pipeline[1] = %#v, want the non-required make lint step", packet.Pipeline[1])
	}
}

// A developer execution whose agent succeeded but whose required pipeline
// command failed must not proceed to delivery, and (repair loop off) must end
// the run at StatusBlocked with the failing command named in blocking_reason
// -- the deterministic completion gate PROJECT.md describes, verified end to
// end rather than merely asserted from packet shape.
func TestPipelineFailureGatesDeliveryAndBlocksByDefault(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	payload := `{"status":"completed","exitCode":0,"pipelineResults":[{"command":"make test","exitCode":1,"output":"FAIL"}]}`
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (pipeline failure): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked; the repair loop defaults to off", state.status)
	}
	if !strings.Contains(state.blocking, "make test") {
		t.Fatalf("blocking_reason = %q, want it to name the failing command", state.blocking)
	}
	if attempts := h.pipelineRepairAttempts(workflowID); attempts != 0 {
		t.Fatalf("pipeline_repair_attempts = %d, want 0", attempts)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a blocked run must release its project lock")
	}
}

// The opt-in path: a pipeline failure on a project with enable_repair_loop set
// must dispatch a bounded repair attempt instead of blocking -- reopening the
// one job back to 'developer' (it never left that role), moving the run to
// StatusRepairing, counting the attempt in pipeline_repair_attempts (not
// ci_repair_attempts, which must stay untouched), and handing the reopened
// execution the failing command as its one reviewFindings entry.
func TestPipelineFailureDispatchesABoundedRepairAttempt(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableRepairLoop(projectID)
	h.addPipelineStep(projectID, "make test", 300, true)
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	jobID, workflowID := h.runJob(runnerID)
	h.drainOffer(outbound) // the original developer offer

	payload := `{"status":"completed","exitCode":0,"pipelineResults":[{"command":"make test","exitCode":1,"output":"FAIL"}]}`
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (pipeline failure): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "repairing" {
		t.Fatalf("status = %q, want repairing", state.status)
	}
	if attempts := h.pipelineRepairAttempts(workflowID); attempts != 1 {
		t.Fatalf("pipeline_repair_attempts = %d, want 1", attempts)
	}
	if attempts := h.ciRepairAttempts(workflowID); attempts != 0 {
		t.Fatalf("ci_repair_attempts = %d, want 0 unchanged; a pipeline-triggered repair must spend its own counter", attempts)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer", role)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 1 {
		t.Fatal("a run under repair is still active work and must keep its project lock")
	}

	offer := h.readOffer(outbound)
	var packet struct {
		Role           string           `json:"role"`
		ReviewFindings []string         `json:"reviewFindings"`
		Pipeline       []map[string]any `json:"pipeline"`
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
	if len(packet.ReviewFindings) != 1 || !strings.Contains(packet.ReviewFindings[0], "make test") {
		t.Fatalf("packet reviewFindings = %v, want the failing command named", packet.ReviewFindings)
	}
	if len(packet.Pipeline) != 1 {
		t.Fatalf("repair packet pipeline = %#v, want the project's configured step carried forward", packet.Pipeline)
	}
}

// The bound must actually be enforced: once a run has already spent
// max_repair_attempts on pipeline failures, a further failure must fall
// through to blocking exactly like the opted-out case, not repair forever.
func TestPipelineRepairBoundEnforcedFallsThroughToBlocking(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableRepairLoop(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	h.exec(`UPDATE app.workflow_runs SET pipeline_repair_attempts=$1 WHERE id=$2`, maxRepairAttempts, workflowID)

	payload := `{"status":"completed","exitCode":0,"pipelineResults":[{"command":"make test","exitCode":1,"output":"still failing"}]}`
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent (pipeline failure): %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked; the repair bound was already spent", state.status)
	}
	if !strings.Contains(state.blocking, "make test") {
		t.Fatalf("blocking_reason = %q, want it to carry the failing command", state.blocking)
	}
	if attempts := h.pipelineRepairAttempts(workflowID); attempts != int(maxRepairAttempts) {
		t.Fatalf("pipeline_repair_attempts = %d, want unchanged at %d; a blocked run must not spend an attempt it never used", attempts, maxRepairAttempts)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer; nothing should have reopened it past the bound", role)
	}
}

// If the process dies (or no runner was connected) between persistExecutionEvent
// committing a pipeline failure's terminal event and pipelineFailedOrBlock's own
// follow-on decision, the recovery sweep must apply the same repair decision
// the inline path would have.
func TestRecoverySweepAppliesPipelineRepairForAStrandedFailure(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.enableRepairLoop(projectID)
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	h.exec(`UPDATE app.jobs SET status='failed', finished_at=now() WHERE id=$1`, jobID)
	h.exec(`UPDATE app.workflow_runs SET status='pipeline_failed', current_phase='pipeline_failed', blocking_reason='required pipeline command failed with exit code 1: make test', updated_at=now()-interval '10 minutes' WHERE id=$1`, workflowID)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "repairing" {
		t.Fatalf("status = %q, want repairing; the stranded pipeline failure should have been repaired", state.status)
	}
	if attempts := h.pipelineRepairAttempts(workflowID); attempts != 1 {
		t.Fatalf("pipeline_repair_attempts = %d, want 1", attempts)
	}
	if role := h.jobRole(jobID); role != "developer" {
		t.Fatalf("job role = %q, want developer", role)
	}
}
