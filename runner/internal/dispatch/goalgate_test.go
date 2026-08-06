package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

// scriptedTurn is one agent invocation's scripted outcome.
type scriptedTurn struct {
	result agents.Result
	err    error
}

// scriptedBackend answers each invocation from a script, repeating the last
// turn once the script runs out so a test asserts the continuation loop's own
// bounds rather than the length of its fixture. It records every request and
// whether it arrived through Execute or Continue.
type scriptedBackend struct {
	turns    []scriptedTurn
	requests []agents.Request
	resumed  []bool
	// onInvoke runs at the start of each invocation, so a test can act on the
	// workspace exactly as a real agent process would.
	onInvoke func(request agents.Request, resumed bool)
}

func (backend *scriptedBackend) Name() string                      { return "scripted" }
func (backend *scriptedBackend) HealthCheck(context.Context) error { return nil }
func (backend *scriptedBackend) Cancel(string) error               { return nil }

func (backend *scriptedBackend) Execute(_ context.Context, request agents.Request) (agents.Result, error) {
	return backend.record(request, false)
}

func (backend *scriptedBackend) Continue(_ context.Context, request agents.Request) (agents.Result, error) {
	return backend.record(request, true)
}

func (backend *scriptedBackend) record(request agents.Request, resumed bool) (agents.Result, error) {
	if backend.onInvoke != nil {
		backend.onInvoke(request, resumed)
	}
	index := len(backend.requests)
	backend.requests = append(backend.requests, request)
	backend.resumed = append(backend.resumed, resumed)
	if index >= len(backend.turns) {
		index = len(backend.turns) - 1
	}
	return backend.turns[index].result, backend.turns[index].err
}

func (backend *scriptedBackend) continuations() int {
	count := 0
	for _, resumed := range backend.resumed {
		if resumed {
			count++
		}
	}
	return count
}

// repeatingInspector reports each snapshot in turn and then repeats the last,
// so a test can describe a workspace that keeps looking the same however many
// times the gate measures it.
type repeatingInspector struct {
	summaries []repository.RevisionSummary
	calls     int
}

func (inspector *repeatingInspector) Snapshot(context.Context, repository.Workspace) (repository.RevisionSummary, error) {
	index := inspector.calls
	inspector.calls++
	if index >= len(inspector.summaries) {
		index = len(inspector.summaries) - 1
	}
	return inspector.summaries[index], nil
}

func developerLease() control.Lease {
	lease := validLease()
	lease.Packet.Constraints = taskpacket.Constraints{MayModifyFiles: true}
	return lease
}

// A developer that stops after half the work — a completed result document that
// still lists remaining work — is re-prompted inside the same execution and can
// finish, with no orchestrator involvement: one lease, one Execute call, and one
// terminal Result.
func TestAgentThatStopsHalfWayIsContinuedAndFinishesInTheSameExecution(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "wrote the parser", RemainingWork: []string{"write the tests"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "tests written", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          agent,
		Delivery:         &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations: 3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{
			{Revision: "base"},
			{Revision: "base", ChangedFiles: []string{"parser.go"}},
		}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || result.Continuations != 1 || result.GateVerdict != "delivered" {
		t.Fatalf("result = %#v", result)
	}
	if len(agent.requests) != 2 || agent.resumed[0] || !agent.resumed[1] {
		t.Fatalf("agent invocations = %d, resumed = %#v", len(agent.requests), agent.resumed)
	}
	continuation := agent.requests[1]
	if continuation.SessionID != "session-1" {
		t.Fatalf("continuation did not resume the captured session: %q", continuation.SessionID)
	}
	for _, want := range []string{"# CONTINUATION", "# MISSING EVIDENCE", "write the tests", "Continue from where you left off", "Implement the task", ".loop/result.json"} {
		if !strings.Contains(continuation.Prompt, want) {
			t.Fatalf("continuation prompt is missing %q:\n%s", want, continuation.Prompt)
		}
	}
}

// A developer role that reports a clean completion while having changed nothing
// has not delivered, whatever its document says.
func TestEmptyDiffFromADeveloperRoleIsContinued(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "nothing to do", SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "implemented", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          agent,
		Delivery:         &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations: 3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{
			{Revision: "base"}, // initial
			{Revision: "base"}, // gate after the first run: nothing changed
			{Revision: "base", ChangedFiles: []string{"implemented.go"}}, // gate after the continuation
		}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Continuations != 1 || result.GateVerdict != "delivered" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(agent.requests[1].Prompt, "No file in the repository was changed") {
		t.Fatalf("continuation prompt did not name the missing diff:\n%s", agent.requests[1].Prompt)
	}
}

// A role that may not modify files is expected to produce no diff, so check (d)
// never applies to it and a clean review is delivered on the first run.
func TestReadOnlyRoleSkipsTheDiffCheck(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	lease := validLease()
	lease.Packet.Role = taskpacket.RoleReviewer
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "approved", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base"}}},
	}

	result, err := dispatcher.Execute(context.Background(), lease)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Continuations != 0 || result.GateVerdict != "delivered" || agent.continuations() != 0 {
		t.Fatalf("result = %#v, continuations = %d", result, agent.continuations())
	}
}

// The loop guard: an agent that reproduces the same verdict is wedged, and the
// run ends after the single continuation that established the repetition rather
// than spending the whole budget on it.
func TestIdenticalVerdictsStopTheLoopBeforeTheBudget(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "partially done", RemainingWork: []string{"write the tests"}, SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Continuations != 1 || agent.continuations() != 1 {
		t.Fatalf("continuations = %d (backend saw %d), want 1", result.Continuations, agent.continuations())
	}
	if !strings.Contains(result.GateVerdict, outcomeRepeatedVerdict) || !strings.Contains(result.GateVerdict, "remaining work") {
		t.Fatalf("GateVerdict = %q", result.GateVerdict)
	}
}

// A verdict that keeps changing never trips the guard, so the budget is what
// bounds the loop — exactly MaxContinuations continuations and no more.
func TestChangingVerdictsExhaustTheContinuationBudget(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	turns := make([]scriptedTurn, 0, 4)
	for attempt := range 4 {
		turns = append(turns, scriptedTurn{result: agents.Result{
			Status:        "completed",
			Summary:       "still going",
			RemainingWork: []string{fmt.Sprintf("step %d", attempt)},
			SessionID:     "session-1",
		}})
	}
	agent := &scriptedBackend{turns: turns}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Continuations != 3 || len(agent.requests) != 4 {
		t.Fatalf("continuations = %d, invocations = %d", result.Continuations, len(agent.requests))
	}
	if !strings.Contains(result.GateVerdict, outcomeBudget) {
		t.Fatalf("GateVerdict = %q", result.GateVerdict)
	}
}

// The dependency this layer rests on (#89): a clean exit with no result document
// is not a success, so it is exactly the kind of early stop the gate continues.
// The continuation supplies the evidence and the execution delivers.
func TestMissingResultDocumentIsContinuedAndCanRecover(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "failed"}, err: fmt.Errorf("%w (.loop/result.json): agent result was not written", agents.ErrNoResultEvidence)},
		{result: agents.Result{Status: "completed", Summary: "implemented", SessionID: "session-2"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          agent,
		Delivery:         &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations: 3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{
			{Revision: "base"},
			{Revision: "base", ChangedFiles: []string{"parser.go"}},
		}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || result.Continuations != 1 || result.SessionID != "session-2" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(agent.requests[1].Prompt, "No result document was written at .loop/result.json") {
		t.Fatalf("continuation prompt did not name the missing document:\n%s", agent.requests[1].Prompt)
	}
	// Without a session to resume the continuation still runs, as a fresh run
	// driven by the continuation prompt.
	if !agent.resumed[1] || agent.requests[1].SessionID != "" {
		t.Fatalf("continuation = resumed %v, session %q", agent.resumed[1], agent.requests[1].SessionID)
	}
}

// With the budget at zero the goal loop is off and an execution is one agent
// launch, exactly as before this layer existed.
func TestZeroBudgetLeavesExecutionSingleShot(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "done", RemainingWork: []string{"more"}, SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	result, err := dispatcher.Execute(context.Background(), validLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(agent.requests) != 1 || result.Continuations != 0 || result.GateVerdict != "" {
		t.Fatalf("invocations = %d, result = %#v", len(agent.requests), result)
	}
}

// #410: the configured agent silence bound travels on every request the goal
// loop makes, first attempt and continuation alike, so each attempt is bounded
// by the same silence and a wedged agent is re-engaged rather than waited out.
func TestAgentSilenceBoundTravelsOnEveryRequest(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "a", RemainingWork: []string{"one"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "b", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		AgentSilence:      4 * time.Minute,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	if _, err := dispatcher.Execute(context.Background(), developerLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(agent.requests) != 2 {
		t.Fatalf("invocations = %d, want 2", len(agent.requests))
	}
	for index, request := range agent.requests {
		if request.Silence != 4*time.Minute {
			t.Fatalf("request %d Silence = %v, want 4m", index, request.Silence)
		}
	}
}

// The packet's timeoutSeconds bounds the *total* agent wall clock. The first
// invocation still gets the whole of it, and each continuation gets only what
// is left, so continuations cannot extend an execution beyond the bound the
// orchestrator set and the lease was sized against.
func TestContinuationsShareThePacketTimeout(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "a", RemainingWork: []string{"one"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "b", RemainingWork: []string{"two"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "c", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	if _, err := dispatcher.Execute(context.Background(), developerLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(agent.requests) != 3 {
		t.Fatalf("invocations = %d, want 3", len(agent.requests))
	}
	packetTimeout := 600 * time.Second
	if agent.requests[0].Timeout != packetTimeout {
		t.Fatalf("first invocation timeout = %v, want the packet timeout %v", agent.requests[0].Timeout, packetTimeout)
	}
	for index := 1; index < len(agent.requests); index++ {
		if agent.requests[index].Timeout >= agent.requests[index-1].Timeout {
			t.Fatalf("continuation %d timeout %v did not shrink from %v", index, agent.requests[index].Timeout, agent.requests[index-1].Timeout)
		}
		if agent.requests[index].Timeout <= 0 || agent.requests[index].Timeout > packetTimeout {
			t.Fatalf("continuation %d timeout %v is outside the packet budget", index, agent.requests[index].Timeout)
		}
	}
}

// A packet whose remaining wall clock cannot fund another agent run stops the
// loop rather than launching a process only to signal it.
func TestExhaustedTimeBudgetStopsTheLoop(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	lease := developerLease()
	lease.Packet.TimeoutSeconds = 1
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"more"}, SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), lease)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Continuations != 0 || !strings.Contains(result.GateVerdict, outcomeTimeBudget) {
		t.Fatalf("result = %#v", result)
	}
}

// A cancelled execution is not answered with another agent process.
func TestCancelledExecutionIsNotContinued(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"more"}, SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, _ := dispatcher.Execute(ctx, developerLease())
	if result.Continuations != 0 || agent.continuations() != 0 {
		t.Fatalf("a cancelled execution was continued: %#v", result)
	}
}

// An agent-declared block earns one continuation — the blocker is sometimes
// clearable once asked — and no more, because re-declaring it reproduces the
// verdict and trips the guard.
func TestDeclaredBlockIsContinuedOnceThenReported(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "blocked", Summary: "the deployment credential is missing", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "blocked" || result.Continuations != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(agent.requests[1].Prompt, "the deployment credential is missing") {
		t.Fatalf("continuation prompt did not quote the block:\n%s", agent.requests[1].Prompt)
	}
	if !strings.Contains(result.GateVerdict, outcomeRepeatedVerdict) {
		t.Fatalf("GateVerdict = %q", result.GateVerdict)
	}
}

// The guard is only useful if identical situations really do hash identically.
// A verdict signature must contain nothing that varies between two evaluations
// of the same state — no timestamp, no absolute path, no map iteration order.
func TestGateSignatureIsStableForIdenticalState(t *testing.T) {
	dispatcher := Dispatcher{
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"b.go", "a.go"}}}},
	}
	packet := developerLease().Packet
	result := agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"second", "first"}}
	workspace := testWorkspace(t)
	initial := repository.RevisionSummary{Revision: "base"}

	first := dispatcher.evaluateGoalGate(context.Background(), packet, workspace, initial, result, nil)
	second := dispatcher.evaluateGoalGate(context.Background(), packet, workspace, initial, result, nil)
	if first.Signature != second.Signature {
		t.Fatalf("signature is unstable: %q vs %q", first.Signature, second.Signature)
	}

	// The changed-file set and the remaining-work list are compared as sets, so
	// a re-ordered report is not mistaken for progress.
	reordered := agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"first", "second"}}
	if third := dispatcher.evaluateGoalGate(context.Background(), packet, workspace, initial, reordered, nil); third.Signature != first.Signature {
		t.Fatalf("re-ordered remaining work changed the signature")
	}

	// Genuine progress does change it.
	progressed := agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"first"}}
	if fourth := dispatcher.evaluateGoalGate(context.Background(), packet, workspace, initial, progressed, nil); fourth.Signature == first.Signature {
		t.Fatal("a shorter remaining-work list produced the same signature")
	}
}

// A gate that cannot measure the diff must not invent a "no changes" verdict:
// an unmeasurable workspace is not evidence of an idle agent.
func TestUnmeasurableDiffSkipsTheChangeCheck(t *testing.T) {
	dispatcher := Dispatcher{RevisionInspector: &failingInspector{}}
	verdict := dispatcher.evaluateGoalGate(
		context.Background(),
		developerLease().Packet,
		testWorkspace(t),
		repository.RevisionSummary{Revision: "base"},
		agents.Result{Status: "completed", Summary: "implemented"},
		nil,
	)
	if !verdict.Delivered {
		t.Fatalf("verdict = %#v, want delivered when the diff cannot be measured", verdict)
	}
}

// Without a revision inspector there is no diff evidence at all, and a missing
// inspector must not manufacture continuations.
func TestMissingRevisionInspectorSkipsTheChangeCheck(t *testing.T) {
	verdict := Dispatcher{}.evaluateGoalGate(
		context.Background(),
		developerLease().Packet,
		testWorkspace(t),
		repository.RevisionSummary{},
		agents.Result{Status: "completed", Summary: "implemented"},
		nil,
	)
	if !verdict.Delivered {
		t.Fatalf("verdict = %#v, want delivered without a revision inspector", verdict)
	}
}

type failingInspector struct{}

func (failingInspector) Snapshot(context.Context, repository.Workspace) (repository.RevisionSummary, error) {
	return repository.RevisionSummary{}, errors.New("worktree is locked")
}

// Every continuation prompt is kept beside the first one, so a retained
// workspace explains why the execution continued and what was asked.
func TestContinuationPromptsAreRecordedInTheWorkspace(t *testing.T) {
	workspace := testWorkspace(t)
	manager := &workspaceManager{workspace: workspace}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "partial", RemainingWork: []string{"finish"}, SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		Retention:         RetentionPolicy{KeepFailed: true, MaxAge: time.Hour, MaxWorkspaces: 4, Directory: t.TempDir()},
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	if _, err := dispatcher.Execute(context.Background(), developerLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace.Repository, ".loop", "continuation-1.md"))
	if err != nil {
		t.Fatalf("continuation prompt was not recorded: %v", err)
	}
	if !strings.Contains(string(contents), "# MISSING EVIDENCE") {
		t.Fatalf("recorded continuation prompt = %q", contents)
	}
}

// The terminal payload is where the orchestrator learns the difference between
// "delivered after two continuations" and "gave up after three".
func TestTerminalPayloadReportsContinuationsAndVerdict(t *testing.T) {
	payload := terminalPayload("completed", Result{Status: "completed", Continuations: 2, GateVerdict: outcomeDelivered}, map[string]any{})
	if payload["continuations"] != 2 || payload["gateVerdict"] != outcomeDelivered {
		t.Fatalf("payload = %#v", payload)
	}
	// They survive the reduction an oversized payload falls back to, since a
	// verbose agent is exactly the case where the count still has to arrive.
	reduced := minimalTerminalPayload(map[string]any{
		"status":        "failed",
		"continuations": 3,
		"gateVerdict":   "not delivered (" + outcomeBudget + "): the agent reported remaining work",
		"changedFiles":  []string{"a.go"},
	})
	if reduced["continuations"] != 3 || reduced["gateVerdict"] == nil {
		t.Fatalf("reduced payload = %#v", reduced)
	}

	// A run that never continued reports neither field rather than a zero.
	quiet := terminalPayload("completed", Result{Status: "completed"}, map[string]any{})
	if _, present := quiet["continuations"]; present {
		t.Fatalf("payload = %#v", quiet)
	}
}

// A continuation must be judged on evidence it produced itself. The previous
// attempt's result document is set aside before the continuation starts —
// otherwise a continuation that writes nothing is assessed against its
// predecessor's document and can be declared delivered on evidence it never
// produced, which is the "a clean exit is not a result" hole (#89) re-opened
// inside one execution.
func TestPreviousResultDocumentIsSetAsideBeforeAContinuation(t *testing.T) {
	workspace := testWorkspace(t)
	manager := &workspaceManager{workspace: workspace}
	documentPath := filepath.Join(workspace.Repository, ".loop", "result.json")
	var documentPresentOnContinuation bool
	agent := &scriptedBackend{
		turns: []scriptedTurn{
			{result: agents.Result{Status: "completed", Summary: "nothing to do", SessionID: "session-1"}},
			{result: agents.Result{Status: "completed", Summary: "implemented", SessionID: "session-1"}},
		},
		onInvoke: func(_ agents.Request, resumed bool) {
			if resumed {
				_, err := os.Stat(documentPath)
				documentPresentOnContinuation = err == nil
				return
			}
			// The first attempt writes a document, as a real agent would.
			if err := os.WriteFile(documentPath, []byte(`{"protocolVersion":"1.0"}`), 0o600); err != nil {
				t.Errorf("write result document: %v", err)
			}
		},
	}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          agent,
		Delivery:         &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations: 3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{
			{Revision: "base"},
			{Revision: "base"},
			{Revision: "base", ChangedFiles: []string{"implemented.go"}},
		}},
	}

	if _, err := dispatcher.Execute(context.Background(), developerLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if documentPresentOnContinuation {
		t.Fatal("the continuation inherited the previous attempt's result document")
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, ".loop", "result-attempt-1.json")); err != nil {
		t.Fatalf("the previous attempt's document was not retained: %v", err)
	}
}

// A continuation exists to improve on the attempt before it and must never make
// it worse. A crashed continuation after a completed first attempt reports the
// completed one — the outcome the same execution reaches with the goal loop
// switched off — rather than replacing a delivery with an anonymous failure.
func TestAFailingContinuationCannotDestroyACompletedAttempt(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "implemented", RemainingWork: []string{"polish the docs"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "failed"}, err: errors.New("command exited unsuccessfully: signal: killed")},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || result.Summary != "implemented" {
		t.Fatalf("a failing continuation destroyed the completed attempt: %#v", result)
	}
	if result.Continuations == 0 {
		t.Fatalf("the continuation was not counted: %#v", result)
	}
	if !result.Committed {
		t.Fatalf("the completed attempt was not delivered: %#v", result)
	}
}

// The same rule protects an agent-declared block, whose stated reason is the
// most actionable thing a non-delivering run produces (#97).
func TestAFailingContinuationCannotDestroyADeclaredBlock(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "blocked", Summary: "the deployment credential is missing", RemainingWork: []string{"obtain DEPLOY_KEY"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "failed"}, err: errors.New("command exited unsuccessfully: signal: killed")},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "blocked" || result.Summary != "the deployment credential is missing" {
		t.Fatalf("a failing continuation destroyed the declared block: %#v", result)
	}
	if len(result.RemainingWork) != 1 || result.RemainingWork[0] != "obtain DEPLOY_KEY" {
		t.Fatalf("the block's remaining work was lost: %#v", result)
	}
}

// The continuation prompt carries the role's whole original prompt, not a
// summary of it. Every sessionless backend, and the most common continuation of
// all — no result document, so no session ID was ever captured — starts a fresh
// agent on this prompt, and it must be held to the same role and context the
// first run was.
func TestContinuationPromptCarriesTheWholeOriginalPrompt(t *testing.T) {
	packet := developerLease().Packet
	packet.Plan = []string{"split the parser"}
	packet.PreviousFailures = []string{"attempt 1 timed out"}
	packet.ReviewFindings = []string{"missing error handling"}
	verdict := gateVerdict{Reasons: []gateReason{reasonRemainingWork}, Signature: "x"}
	prompt := continuationPrompt(packet, verdict, agents.Result{RemainingWork: []string{"write the tests"}}, 1, 3)

	for _, want := range []string{
		"# CONTINUATION", "# MISSING EVIDENCE", "write the tests",
		"# ROLE", "# CURRENT PLAN", "split the parser",
		"# PREVIOUS FAILURES", "attempt 1 timed out",
		"# AI REVIEW FINDINGS", "missing error handling",
		"# FORBIDDEN ACTIONS", "# EXPECTED OUTPUT SCHEMA",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("continuation prompt is missing %q:\n%s", want, prompt)
		}
	}

	// A reviewer keeps the independence framing its role prompt exists to
	// impose; a generic continuation prompt would silently drop it.
	packet.Role = taskpacket.RoleReviewer
	reviewerContinuation := continuationPrompt(packet, verdict, agents.Result{}, 1, 3)
	if !strings.Contains(reviewerContinuation, "do not defend") || strings.Contains(reviewerContinuation, "split the parser") {
		t.Fatalf("reviewer continuation prompt lost its independence framing:\n%s", reviewerContinuation)
	}
}

// Clean outcomes are not ranked against each other. An agent that completes and
// then, asked to continue, declares itself blocked has retracted its own claim
// with a stated reason; that retraction is what a retry or an escalation is
// built from, and it must not be overridden by the claim it replaced.
func TestALaterCleanRetractionWinsOverAnEarlierClaim(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "implemented", RemainingWork: []string{"obtain DEPLOY_KEY"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "blocked", Summary: "the deployment credential is missing", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:        manager,
		Backend:           agent,
		Delivery:          &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations:  3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{{Revision: "base", ChangedFiles: []string{"parser.go"}}}},
	}

	result, err := dispatcher.Execute(context.Background(), developerLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "blocked" || result.Summary != "the deployment credential is missing" {
		t.Fatalf("the agent's own retraction was overridden: %#v", result)
	}
}

// Nothing bounds what an agent writes into its result document, and the whole
// continuation prompt travels as one argv element. A verbose agent must not be
// able to grow the prompt until the exec fails, spending a continuation on a
// process that never starts.
func TestContinuationPromptBoundsWhatItQuotesBackFromTheAgent(t *testing.T) {
	packet := developerLease().Packet
	remaining := make([]string, 200)
	for index := range remaining {
		remaining[index] = strings.Repeat("finish the migration ", 200)
	}
	result := agents.Result{Summary: strings.Repeat("blocked because ", 4000), RemainingWork: remaining}
	verdict := gateVerdict{Reasons: []gateReason{reasonAgentBlocked, reasonRemainingWork}, Signature: "x"}

	prompt := continuationPrompt(packet, verdict, result, 1, 3)
	quoted := len(prompt) - len(promptFor(packet))
	if quoted > 16*1024 {
		t.Fatalf("continuation prompt quoted %d bytes of agent text back unbounded", quoted)
	}
	if !strings.Contains(prompt, truncationMarker) {
		t.Fatalf("oversized agent text was not marked as truncated:\n%s", prompt[:2000])
	}
}
