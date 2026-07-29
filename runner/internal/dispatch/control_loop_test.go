package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

type loopClient struct {
	mu          sync.Mutex
	accepted    []string
	rejected    []string
	events      []*runnerv1.ExecutionEvent
	sendErr     error
	disconnects int
}

func (client *loopClient) AcceptOffer(jobID string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.accepted = append(client.accepted, jobID)
	return nil
}

func (client *loopClient) RejectOffer(jobID, reason string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.rejected = append(client.rejected, jobID+":"+reason)
	return nil
}
func (client *loopClient) RenewLease(string, int64, time.Time) error { return nil }
func (client *loopClient) Receive() (*runnerv1.OrchestratorToRunner, error) {
	return nil, control.ErrNotConnected
}
func (client *loopClient) Disconnect() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.disconnects++
}

func (client *loopClient) SendExecutionEvent(event *runnerv1.ExecutionEvent) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.sendErr != nil {
		return client.sendErr
	}
	client.events = append(client.events, event)
	return nil
}

type staticDispatcher struct {
	mu     sync.Mutex
	calls  int
	result Result
	err    error
}

func (dispatcher *staticDispatcher) Execute(context.Context, control.Lease) (Result, error) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	dispatcher.calls++
	return dispatcher.result, dispatcher.err
}

func TestControlLoopDispatchesAcknowledgedLeaseAndReportsTerminalResult(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{Status: "completed", ExitCode: 0, Summary: "implemented the endpoint", ChangedFiles: []string{"main.go"}, CommandsRun: []string{"go test ./..."}}}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}

	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.accepted) != 1 || client.accepted[0] != offer.GetJobId() {
		t.Fatalf("accepted offers = %#v", client.accepted)
	}
	if len(client.events) != 2 {
		t.Fatalf("events = %#v", client.events)
	}
	for index, event := range client.events {
		if event.GetJobId() != offer.GetJobId() || event.GetExecutionId() != "execution-1" || event.GetLeaseGeneration() != offer.GetLeaseGeneration() || event.GetEventSequence() != int64(index+1) {
			t.Fatalf("event %d = %#v", index, event)
		}
	}
	if client.events[0].GetType() != "started" || client.events[1].GetType() != "completed" {
		t.Fatalf("event types = %q, %q", client.events[0].GetType(), client.events[1].GetType())
	}
	if client.events[1].GetPayloadJson() == "" {
		t.Fatal("terminal event carried no payload")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.events[1].GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["durationMs"] == nil || payload["changedFileCount"] != float64(1) || payload["commandCount"] != float64(1) || payload["pipelineCommandCount"] != float64(0) {
		t.Fatalf("terminal usage payload = %#v", payload)
	}
	if payload["summary"] != "implemented the endpoint" {
		t.Fatalf("terminal payload dropped the agent's summary: %#v", payload)
	}
}

// TestControlLoopRedactsSecretsInTheForwardedAgentAccount: the agent's summary
// and remaining work now cross the wire (issue #97), so the reporter's
// redaction has to cover them the same way it covers the result document an
// agent has always been able to write a credential into.
func TestControlLoopRedactsSecretsInTheForwardedAgentAccount(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{
		Status:        "blocked",
		Summary:       "could not authenticate with ghp_abcdef1234567890",
		RemainingWork: []string{"rotate loopsecret_zzz9"},
	}}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, []string{"loopsecret_"}, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	contents := client.events[1].GetPayloadJson()
	if contains(contents, "ghp_abcdef1234567890") || contains(contents, "loopsecret_zzz9") {
		t.Fatalf("terminal payload leaked a credential: %s", contents)
	}
	if !contains(contents, "could not authenticate with") || !contains(contents, "rotate") {
		t.Fatalf("redaction destroyed the agent's explanation: %s", contents)
	}
}

func TestControlLoopReportsFailureWithoutStartingRenewalAcknowledgementTwice(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{ExitCode: 1, Summary: "the agent timed out"}, err: context.DeadlineExceeded}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	acknowledgement := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}
	if err := loop.Handle(context.Background(), acknowledgement); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)
	if err := loop.Handle(context.Background(), acknowledgement); err != nil {
		t.Fatalf("Handle(renewal acknowledgement) error = %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	dispatcher.mu.Lock()
	calls := dispatcher.calls
	dispatcher.mu.Unlock()
	if calls != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", calls)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.events) != 2 || client.events[1].GetType() != "failed" {
		t.Fatalf("failure events = %#v", client.events)
	}
	// The executor's own error is the cause of a failing run, not the agent's
	// account of it, so the error field names the timeout rather than the
	// summary a partially finished agent happened to leave behind.
	if !contains(client.events[1].GetPayloadJson(), context.DeadlineExceeded.Error()) {
		t.Fatalf("failure payload = %s", client.events[1].GetPayloadJson())
	}
}

func TestControlLoopReportsFailedTerminalEventNamingUnresolvableEnvironment(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Delivery:           delivery,
		Environment:        environmentResolver{err: errors.New(`environment reference "GITHUB_TOKEN" is not configured on this runner`)},
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}

	offer := credentialOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	acknowledgement := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}
	if err := loop.Handle(context.Background(), acknowledgement); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.events) != 2 || client.events[1].GetType() != "failed" {
		t.Fatalf("terminal events = %#v", client.events)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.events[1].GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	message, _ := payload["error"].(string)
	if !strings.Contains(message, "GITHUB_TOKEN") || !strings.Contains(message, "not configured") {
		t.Fatalf("failed terminal payload = %#v", payload)
	}
	if len(delivery.commits) != 0 || len(delivery.pushes) != 0 {
		t.Fatalf("runner attempted unauthenticated delivery: %#v", delivery)
	}
}

func credentialOffer(t *testing.T) *runnerv1.JobOffer {
	t.Helper()
	contents, err := json.Marshal(taskpacket.Packet{
		ProtocolVersion: taskpacket.ProtocolVersion,
		JobID:           "job-1", ExecutionID: "execution-1", Role: taskpacket.RoleDeveloper,
		Objective: "Implement task", Issue: taskpacket.Issue{ExternalID: "7", Title: "Task", Body: "Body"},
		Repository:      taskpacket.Repository{ProjectID: "project-1", Mode: "managed_clone", URL: "https://github.com/owner/repo.git", DefaultBranch: "main", Branch: "agent/issue-7/run-1"},
		PromptPath:      ".loop/prompt.md",
		ExpectedOutput:  ".loop/result.json",
		TimeoutSeconds:  60,
		EnvironmentRefs: []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}},
		Constraints:     taskpacket.Constraints{MayModifyFiles: true, MayPush: true},
	})
	if err != nil {
		t.Fatalf("encode packet: %v", err)
	}
	return &runnerv1.JobOffer{JobId: "job-1", LeaseGeneration: 1, TaskPacketJson: string(contents)}
}

func TestControlLoopLogsOfferCorrelationFields(t *testing.T) {
	client := &loopClient{}
	loop, err := NewControlLoopWithOutbox(client, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	loop.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("parse structured log: %v", err)
	}
	if entry["msg"] != "runner received job offer" || entry["job_id"] != offer.GetJobId() || entry["lease_generation"] != float64(offer.GetLeaseGeneration()) {
		t.Fatalf("structured log = %#v", entry)
	}
}

func loopOffer(t *testing.T) *runnerv1.JobOffer {
	t.Helper()
	return loopOfferFor(t, "job-1", "execution-1", "project-1")
}

func loopOfferFor(t *testing.T, jobID, executionID, projectID string) *runnerv1.JobOffer {
	t.Helper()
	contents, err := json.Marshal(taskpacket.Packet{
		ProtocolVersion: taskpacket.ProtocolVersion,
		JobID:           jobID, ExecutionID: executionID, Role: taskpacket.RoleDeveloper,
		Objective: "Implement task", Issue: taskpacket.Issue{ExternalID: "7", Title: "Task", Body: "Body"},
		Repository: taskpacket.Repository{ProjectID: projectID, Mode: "managed_clone", URL: "https://example.test/repo.git", DefaultBranch: "main", Branch: "agent/issue-7/run-1"},
		PromptPath: ".loop/prompt.md", ExpectedOutput: ".loop/result.json", TimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("encode packet: %v", err)
	}
	return &runnerv1.JobOffer{JobId: jobID, LeaseGeneration: 1, TaskPacketJson: string(contents)}
}

func waitForEvents(t *testing.T, client *loopClient, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		length := len(client.events)
		client.mu.Unlock()
		if length == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for execution events")
}

func contains(value, fragment string) bool {
	return strings.Contains(value, fragment)
}

type blockingDispatcher struct {
	started   chan struct{}
	cancelled chan control.Lease
}

func (dispatcher *blockingDispatcher) Execute(ctx context.Context, _ control.Lease) (Result, error) {
	close(dispatcher.started)
	<-ctx.Done()
	return Result{ExitCode: -1}, ctx.Err()
}

func (dispatcher *blockingDispatcher) Cancel(_ context.Context, lease control.Lease) error {
	dispatcher.cancelled <- lease
	return nil
}

func TestControlLoopCancelsMatchingActiveExecution(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Cancel{Cancel: &runnerv1.CancelExecution{ExecutionId: "execution-1", LeaseGeneration: 1}}}); err != nil {
		t.Fatalf("Handle(cancel) error = %v", err)
	}
	select {
	case lease := <-dispatcher.cancelled:
		if lease.JobID != offer.GetJobId() {
			t.Fatalf("cancelled lease = %#v", lease)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher was not cancelled")
	}
	waitForEvents(t, client, 2)
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.events[1].GetType() != "cancelled" {
		t.Fatalf("events = %#v", client.events)
	}
	deadline := time.Now().Add(time.Second)
	for loop.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if loop.Busy() {
		t.Fatal("loop remained busy after cancelled execution completed")
	}
}

func TestControlLoopDrainRejectsNewOffer(t *testing.T) {
	client := &loopClient{}
	loop, err := NewControlLoopWithOutbox(client, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	loop.Drain()
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: loopOffer(t)}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if !loop.Draining() || len(client.accepted) != 0 || len(client.rejected) != 1 || client.rejected[0] != "job-1:runner is draining" {
		t.Fatalf("accepted = %#v, rejected = %#v", client.accepted, client.rejected)
	}
}

func TestControlLoopDrainKeepsBusyExecutionUntilTerminal(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	loop.Drain()
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: loopOffer(t)}}); err != nil {
		t.Fatalf("Handle(new offer) error = %v", err)
	}
	select {
	case <-dispatcher.cancelled:
		t.Fatal("drain cancelled the active execution")
	default:
	}
	if !loop.Busy() {
		t.Fatal("draining runner lost its active execution")
	}
	client.mu.Lock()
	rejected := append([]string(nil), client.rejected...)
	client.mu.Unlock()
	if len(rejected) != 1 || rejected[0] != "job-1:runner is draining" {
		t.Fatalf("rejected = %#v", rejected)
	}
	loop.Cancel("execution-1", 1)
	waitForEvents(t, client, 2)
}

func TestControlLoopRecoversLeaseLossByCancellingExecution(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Second).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	now = now.Add(2 * time.Second)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	select {
	case lease := <-dispatcher.cancelled:
		if lease.JobID != offer.GetJobId() || lease.Generation != offer.GetLeaseGeneration() {
			t.Fatalf("cancelled lease = %#v", lease)
		}
	case <-time.After(time.Second):
		t.Fatal("expired lease did not cancel execution")
	}
	deadline := time.Now().Add(time.Second)
	for loop.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if loop.Busy() {
		t.Fatal("expired lease remained active")
	}
}

func TestControlLoopExpiresLeaseWhileControlStreamIsDisconnected(t *testing.T) {
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, time.Now, 20*time.Millisecond, 5*time.Millisecond, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	loop.ExpiryInterval = time.Millisecond
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatal(err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: time.Now().Add(10 * time.Millisecond).UnixMilli()}}}); err != nil {
		t.Fatal(err)
	}
	<-dispatcher.started
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = loop.Run(ctx) }()
	select {
	case <-dispatcher.cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnected control stream did not expire lease")
	}
}

// TestTerminalPayloadCarriesTheAgentAccountForEveryOutcome pins issue #97's
// core contract: the agent's own account of what happened — the result
// document, its summary, and what it says still has to be done — travels with
// every terminal outcome, not only with a success.
func TestTerminalPayloadCarriesTheAgentAccountForEveryOutcome(t *testing.T) {
	raw := map[string]any{"status": "blocked", "summary": "needs a credential"}
	account := Result{Raw: raw, Summary: "needs a credential", RemainingWork: []string{"obtain DEPLOY_KEY"}}
	for _, status := range []string{"completed", "failed", "blocked"} {
		payload := terminalPayload(status, account, nil)
		result, ok := payload["result"].(map[string]any)
		if !ok || result["summary"] != "needs a credential" {
			t.Fatalf("terminalPayload(%s) result = %#v", status, payload["result"])
		}
		if payload["summary"] != "needs a credential" {
			t.Fatalf("terminalPayload(%s) summary = %#v", status, payload["summary"])
		}
		remaining, ok := payload["remainingWork"].([]string)
		if !ok || len(remaining) != 1 || remaining[0] != "obtain DEPLOY_KEY" {
			t.Fatalf("terminalPayload(%s) remainingWork = %#v", status, payload["remainingWork"])
		}
	}

	// A cancelled run reached no outcome of its own, so it reports none. This
	// also keeps repeated cancellations identifiable: the orchestrator derives
	// their fingerprint from a stable `cancelled exit=N` text precisely when no
	// `error` or `summary` is present, and an interrupted agent's summary varies
	// from run to run.
	cancelled := terminalPayload("cancelled", account, nil)
	for _, key := range []string{"result", "summary", "remainingWork"} {
		if _, present := cancelled[key]; present {
			t.Fatalf("cancelled terminal payload carried %q: %#v", key, cancelled)
		}
	}

	empty := terminalPayload("completed", Result{}, nil)
	for _, key := range []string{"result", "summary", "remainingWork"} {
		if _, present := empty[key]; present {
			t.Fatalf("terminalPayload with nothing to report carried %q: %#v", key, empty)
		}
	}
}

// TestTerminalPayloadBoundsTheAgentAccount keeps the newly forwarded fields
// inside the reduced-payload budget. `summary` and `remainingWork` are written
// by the agent and are therefore unbounded at the source; the reduced retry
// keeps both, so they have to be bounded where they are built rather than only
// where they are re-emitted.
//
// The fill is deliberately hostile in the way real agent output is: raw byte
// length is the wrong budget because Go's encoder spends six bytes on each `<`,
// `>`, `&`, and control byte, and invalid UTF-8 expands too. Filling with
// characters that encode 1:1 would let a raw-measured bound pass this test and
// still lose the terminal event in production.
func TestTerminalPayloadBoundsTheAgentAccount(t *testing.T) {
	for _, fill := range []struct {
		name string
		text string
	}{
		{name: "plain", text: strings.Repeat("s", 32*1024)},
		{name: "angle brackets", text: strings.Repeat("<div>", 8*1024)},
		{name: "invalid utf-8", text: strings.Repeat("blocked on\xff\xfe", 4096)},
		{name: "ansi escapes", text: strings.Repeat("\x1b[31mFAIL\x1b[0m\n", 2048)},
	} {
		t.Run(fill.name, func(t *testing.T) {
			remaining := make([]string, 200)
			for index := range remaining {
				remaining[index] = fill.text
			}
			payload := terminalPayload("blocked", Result{
				Summary:              fill.text,
				RemainingWork:        remaining,
				LogTail:              fill.text,
				WorkInProgressCommit: "cafebabe",
				WorkInProgressBranch: "wip/execution-1",
				Raw:                  map[string]any{"status": "blocked", "transcript": fill.text},
			}, executionUsage(time.Now(), Result{}))
			// `error` and `failureFingerprint` are attached by execute, not by
			// terminalPayload, and `error` is the largest contributor on the
			// failure path — a bounding test that omits it proves nothing.
			payload["blocked"] = true
			payload["failureFingerprint"] = "execution:0123456789abcdef"
			payload["error"] = fill.text

			if summary, ok := payload["summary"].(string); !ok || jsonEncodedSize(summary) > maxTerminalPayloadFieldBytes {
				t.Fatalf("summary encodes to %d bytes", jsonEncodedSize(summary))
			}
			items, ok := payload["remainingWork"].([]string)
			if !ok || len(items) > maxTerminalPayloadListItems+1 {
				t.Fatalf("remainingWork = %#v", payload["remainingWork"])
			}
			total := 0
			for _, item := range items {
				total += jsonEncodedSize(item)
			}
			if total > maxTerminalPayloadListBytes+jsonEncodedSize(truncationMarker)+32 {
				t.Fatalf("remainingWork encodes to %d bytes", total)
			}

			// The whole reduced payload has to stay inside the encoded event
			// limit, which is the only thing the retry exists to get under.
			reduced := minimalTerminalPayload(payload)
			if reduced == nil {
				t.Fatal("minimalTerminalPayload() did not reduce an oversized agent account")
			}
			encoded, err := json.Marshal(reduced)
			if err != nil {
				t.Fatalf("encode reduced payload: %v", err)
			}
			if len(encoded) > maxEncodedEventPayloadBytes {
				t.Fatalf("reduced payload is %d encoded bytes, over the %d-byte event limit", len(encoded), maxEncodedEventPayloadBytes)
			}
			if _, present := reduced["result"]; present {
				t.Fatalf("reduced payload kept the unbounded result document: %#v", reduced)
			}
		})
	}
}

// maxEncodedEventPayloadBytes mirrors control.maxExecutionEventPayloadBytes,
// which is unexported. TestReducedTerminalPayloadIsAcceptedByTheEventReporter
// pins the two together against the real reporter.
const maxEncodedEventPayloadBytes = 16 * 1024

// TestReducedTerminalPayloadIsAcceptedByTheEventReporter is the end-to-end
// guard: a blocked agent whose every text field is hostile still gets its
// outcome delivered, because the reduced retry fits the reporter's real limit.
// Before the fields were measured as JSON encodes them, this payload was
// rejected twice and the run's outcome was logged as lost.
func TestReducedTerminalPayloadIsAcceptedByTheEventReporter(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	// Every byte here costs six encoded, so a raw-measured 2 KiB bound on
	// `error` alone already spends 12 KiB of the 16 KiB event budget.
	hostile := strings.Repeat("<", 32*1024)
	remaining := make([]string, 50)
	for index := range remaining {
		remaining[index] = hostile
	}
	dispatcher := &staticDispatcher{result: Result{
		Status:        "blocked",
		Summary:       hostile,
		RemainingWork: remaining,
		LogTail:       hostile,
		Raw:           map[string]any{"status": "blocked", "transcript": hostile},
	}}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	terminal := client.events[1]
	if terminal.GetType() != "failed" {
		t.Fatalf("terminal event = %#v, want the outcome to survive a hostile payload", terminal)
	}
	if len(terminal.GetPayloadJson()) > maxEncodedEventPayloadBytes {
		t.Fatalf("delivered payload is %d bytes", len(terminal.GetPayloadJson()))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(terminal.GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	// The block survives the reduction; only its detail is shed.
	if payload["status"] != "blocked" || payload["blocked"] != true {
		t.Fatalf("the reduced payload lost the block: %#v", payload)
	}
	if summary, _ := payload["summary"].(string); summary == "" {
		t.Fatalf("the reduced payload lost the agent's summary: %#v", payload)
	}
	if remainingWork, _ := payload["remainingWork"].([]any); len(remainingWork) == 0 {
		t.Fatalf("the reduced payload lost the remaining work: %#v", payload)
	}
}

// TestMinimalTerminalPayloadKeepsTheBlockedExplanation: the reduced retry runs
// exactly when a payload was too large, which is when a verbose agent blocked.
// Shedding the explanation there would leave the orchestrator with the
// anonymous failure this issue exists to remove.
func TestMinimalTerminalPayloadKeepsTheBlockedExplanation(t *testing.T) {
	reduced := minimalTerminalPayload(map[string]any{
		"status":        "blocked",
		"blocked":       true,
		"summary":       "needs a credential",
		"remainingWork": []string{"obtain DEPLOY_KEY"},
		"result":        map[string]any{"status": "blocked"},
	})
	if reduced == nil {
		t.Fatal("minimalTerminalPayload() did not reduce a payload carrying a result document")
	}
	if reduced["status"] != "blocked" || reduced["blocked"] != true || reduced["summary"] != "needs a credential" {
		t.Fatalf("reduced payload lost the block: %#v", reduced)
	}
	remaining, ok := reduced["remainingWork"].([]string)
	if !ok || len(remaining) != 1 {
		t.Fatalf("reduced payload lost the remaining work: %#v", reduced)
	}
	if _, present := reduced["result"]; present {
		t.Fatalf("reduced payload kept the unbounded result document: %#v", reduced)
	}
}

// TestControlLoopReportsAnAgentReportedBlockDistinctlyFromAFailure is the
// end-to-end shape of issue #97 on the wire.
func TestControlLoopReportsAnAgentReportedBlockDistinctlyFromAFailure(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{
		Status:        "blocked",
		ExitCode:      0,
		Summary:       "the deployment credential is missing",
		RemainingWork: []string{"obtain DEPLOY_KEY"},
		Raw:           map[string]any{"status": "blocked", "summary": "the deployment credential is missing"},
	}}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	terminal := client.events[1]
	// The wire vocabulary is unchanged (orchestrator VALID_EVENT_TYPES): the
	// block is a refinement of the `failed` event, carried in the payload.
	if terminal.GetType() != "failed" {
		t.Fatalf("terminal event type = %q, want failed", terminal.GetType())
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(terminal.GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["status"] != "blocked" || payload["blocked"] != true {
		t.Fatalf("terminal payload does not report the block: %#v", payload)
	}
	if payload["summary"] != "the deployment credential is missing" {
		t.Fatalf("terminal payload summary = %#v", payload["summary"])
	}
	remaining, ok := payload["remainingWork"].([]any)
	if !ok || len(remaining) != 1 || remaining[0] != "obtain DEPLOY_KEY" {
		t.Fatalf("terminal payload remainingWork = %#v", payload["remainingWork"])
	}
	result, ok := payload["result"].(map[string]any)
	if !ok || result["status"] != "blocked" {
		t.Fatalf("terminal payload result document = %#v", payload["result"])
	}
	if fingerprint, _ := payload["failureFingerprint"].(string); fingerprint == "" {
		t.Fatalf("terminal payload lost its failure fingerprint: %#v", payload)
	}
}

// TestControlLoopReportsAProcessFailureAsAFailureEvenWhenTheDocumentSaysBlocked:
// a crashed process's own claim about why it stopped is not evidence. Only a
// cleanly finished agent gets to report a block.
func TestControlLoopReportsAProcessFailureAsAFailureEvenWhenTheDocumentSaysBlocked(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{
		result: Result{Status: "blocked", ExitCode: 3, Summary: "needs a credential"},
		err:    errors.New("execute agent: exit status 3"),
	}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.events[1].GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["status"] != "failed" {
		t.Fatalf("terminal payload status = %#v, want failed", payload["status"])
	}
	if _, marked := payload["blocked"]; marked {
		t.Fatalf("a process failure was reported as an agent block: %#v", payload)
	}
}

func TestControlLoopFlushesBufferedEventsAfterReconnectBeforeLeaseExpiry(t *testing.T) {
	now := time.Now()
	client := &loopClient{sendErr: errors.New("control stream disconnected")}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	client.mu.Lock()
	client.sendErr = nil
	client.mu.Unlock()
	if err := loop.FlushEvents(); err != nil {
		t.Fatalf("FlushEvents() error = %v", err)
	}
	client.mu.Lock()
	events := append([]*runnerv1.ExecutionEvent(nil), client.events...)
	client.mu.Unlock()
	if len(events) != 1 || events[0].GetType() != "started" || !loop.Busy() {
		t.Fatalf("events = %#v, busy = %v", events, loop.Busy())
	}
	loop.Cancel("execution-1", 1)
	waitForEvents(t, client, 2)
}

func TestControlLoopSkipsUnsupportedControlMessageInsteadOfErroring(t *testing.T) {
	client := &loopClient{}
	loop, err := NewControlLoop(client, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	var output bytes.Buffer
	loop.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{}); err != nil {
		t.Fatalf("Handle(unsupported) error = %v, want nil", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("parse structured log: %v", err)
	}
	if entry["level"] != "WARN" {
		t.Fatalf("unsupported message log = %#v, want a warning", entry)
	}
}

func TestControlLoopRunBacksOffWithoutDisconnectingWhileUnreachable(t *testing.T) {
	client := &loopClient{}
	loop, err := NewControlLoop(client, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	loop.ReconnectMin = 2 * time.Millisecond
	loop.ReconnectMax = 8 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context.DeadlineExceeded", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.disconnects != 0 {
		t.Fatalf("Run() called Disconnect() %d times, want 0 (owned by StreamSupervisor)", client.disconnects)
	}
}

// gatedDispatcher blocks after cancellation until release is closed, so a test
// can interleave lease expiry with the still-running execution's terminal event.
type gatedDispatcher struct {
	started   chan struct{}
	release   chan struct{}
	cancelled chan control.Lease
}

func (dispatcher *gatedDispatcher) Execute(ctx context.Context, _ control.Lease) (Result, error) {
	close(dispatcher.started)
	<-ctx.Done()
	<-dispatcher.release
	return Result{ExitCode: -1}, ctx.Err()
}

func (dispatcher *gatedDispatcher) Cancel(_ context.Context, lease control.Lease) error {
	dispatcher.cancelled <- lease
	return nil
}

type syncBuffer struct {
	mu       sync.Mutex
	contents bytes.Buffer
}

func (buffer *syncBuffer) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.contents.Write(payload)
}

func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.contents.String()
}

func TestControlLoopDeliversTerminalEventAfterLeaseExpiryWhileDisconnected(t *testing.T) {
	now := time.Now()
	outboxPath := filepath.Join(t.TempDir(), "events.json")
	client := &loopClient{sendErr: errors.New("control stream disconnected")}
	dispatcher := &gatedDispatcher{started: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Second, 250*time.Millisecond, nil, outboxPath)
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Second).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}

	now = now.Add(2 * time.Second)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	select {
	case <-dispatcher.cancelled:
	case <-time.After(time.Second):
		t.Fatal("expired lease did not cancel the execution")
	}
	close(dispatcher.release)
	waitForPendingEvents(t, loop, 1)

	contents, err := os.ReadFile(outboxPath)
	if err != nil {
		t.Fatalf("read event outbox: %v", err)
	}
	if !contains(string(contents), `"cancelled"`) {
		t.Fatalf("event outbox after lease expiry = %s, want the terminal event", contents)
	}

	client.mu.Lock()
	client.sendErr = nil
	client.mu.Unlock()
	if err := loop.FlushEvents(); err != nil {
		t.Fatalf("FlushEvents() error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.events) != 1 || client.events[0].GetType() != "cancelled" {
		t.Fatalf("delivered events = %#v, want the terminal event", client.events)
	}
	if client.events[0].GetLeaseGeneration() != offer.GetLeaseGeneration() {
		t.Fatalf("terminal event lost its lease fencing: %#v", client.events[0])
	}
}

// chattyDispatcher floods the event buffer with log output before reporting a
// successful result, reproducing an agent that talks while the stream is down.
type chattyDispatcher struct {
	reporter  *control.EventReporter
	messages  int
	saturated chan struct{}
	release   chan struct{}
}

func (dispatcher *chattyDispatcher) Execute(_ context.Context, lease control.Lease) (Result, error) {
	for index := 0; index < dispatcher.messages; index++ {
		_, _ = dispatcher.reporter.EmitLog(lease.JobID, lease.Generation, "chatty agent output")
	}
	close(dispatcher.saturated)
	<-dispatcher.release
	return Result{Status: "completed", ExitCode: 0}, nil
}

func TestControlLoopDeliversTerminalEventAfterLogsSaturateTheBufferWhileDisconnected(t *testing.T) {
	now := time.Now()
	outboxPath := filepath.Join(t.TempDir(), "events.json")
	client := &loopClient{sendErr: errors.New("control stream disconnected")}
	dispatcher := &chattyDispatcher{messages: 32, saturated: make(chan struct{}), release: make(chan struct{})}
	loop, err := NewControlLoopWithEventBuffer(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, outboxPath, 1, 4)
	if err != nil {
		t.Fatalf("NewControlLoopWithEventBuffer() error = %v", err)
	}
	dispatcher.reporter = loop.Reporter
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.saturated:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not produce its log output")
	}
	if pending := loop.Reporter.Pending(); pending != 4 {
		t.Fatalf("pending events = %d, want a saturated buffer of 4", pending)
	}
	close(dispatcher.release)
	waitForOutboxEvent(t, outboxPath, "completed")

	client.mu.Lock()
	client.sendErr = nil
	client.mu.Unlock()
	if err := loop.FlushEvents(); err != nil {
		t.Fatalf("FlushEvents() error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.events) == 0 || client.events[len(client.events)-1].GetType() != "completed" {
		t.Fatalf("delivered events = %#v, want the terminal event last", client.events)
	}
	if _, err := os.Stat(outboxPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("event outbox was not drained: %v", err)
	}
}

func waitForOutboxEvent(t *testing.T, path, eventType string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		contents, err := os.ReadFile(path)
		if err == nil && contains(string(contents), `"`+eventType+`"`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event outbox %s never contained a %q event", path, eventType)
}

func TestControlLoopLogsTerminalEventLoss(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	output := &syncBuffer{}
	loop.Logger = slog.New(slog.NewJSONHandler(output, nil))
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	waitForEvents(t, client, 1)
	if !loop.Reporter.Finish(offer.GetJobId(), offer.GetLeaseGeneration()) {
		t.Fatal("Finish() did not release the event lease")
	}
	loop.Cancel("execution-1", offer.GetLeaseGeneration())
	select {
	case <-dispatcher.cancelled:
	case <-time.After(time.Second):
		t.Fatal("execution was not cancelled")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entry, ok := findLogEntry(t, output.String(), "terminal execution event lost"); ok {
			if entry["level"] != "ERROR" {
				t.Fatalf("terminal event loss logged at %v, want ERROR", entry["level"])
			}
			if entry["job_id"] != offer.GetJobId() || entry["event_type"] != "cancelled" {
				t.Fatalf("terminal event loss log = %#v", entry)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("terminal event loss was not logged: %s", output.String())
}

func TestControlLoopRescuesOversizedTerminalEventWithMinimalPayload(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	oversized := strings.Repeat("x", 20*1024)
	dispatcher := &staticDispatcher{result: Result{Status: "completed", ExitCode: 0, Raw: map[string]any{"transcript": oversized}}}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	terminal := client.events[1]
	if terminal.GetType() != "completed" {
		t.Fatalf("terminal event = %#v, want the outcome to survive an oversized payload", terminal)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(terminal.GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["status"] != "completed" || payload["exitCode"] != float64(0) {
		t.Fatalf("reduced terminal payload lost its classification fields: %#v", payload)
	}
	if _, present := payload["result"]; present {
		t.Fatalf("reduced terminal payload kept the oversized result document: %#v", payload)
	}
}

func TestControlLoopRescuesTerminalEventWithOversizedErrorText(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	// A wedged agent that dumps its stderr into the returned error puts an
	// unbounded string into the one field the reduced payload keeps verbatim.
	oversized := strings.Repeat("x", 26*1024)
	dispatcher := &staticDispatcher{result: Result{Status: "failed", ExitCode: 1}, err: errors.New(oversized)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	terminal := client.events[1]
	if terminal.GetType() != "failed" {
		t.Fatalf("terminal event = %#v, want the outcome to survive an oversized error string", terminal)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(terminal.GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["status"] != "failed" || payload["exitCode"] != float64(1) {
		t.Fatalf("reduced terminal payload lost its classification fields: %#v", payload)
	}
	if payload["failureFingerprint"] == nil || payload["failureFingerprint"] == "" {
		t.Fatalf("reduced terminal payload lost its failure fingerprint: %#v", payload)
	}
	text, ok := payload["error"].(string)
	if !ok {
		t.Fatalf("reduced terminal payload lost its error field: %#v", payload)
	}
	if len(text) >= len(oversized) {
		t.Fatalf("error text was not truncated: %d bytes", len(text))
	}
	if !contains(text, truncationMarker) {
		t.Fatalf("truncated error text is not marked as truncated: %q", text[max(0, len(text)-64):])
	}
}

// TestTerminalPayloadReportsRetainedWorkSeparatelyFromDelivery pins the
// delivery-safety contract on the wire: a failed run names where its work
// survived without ever populating the delivered-branch field.
func TestTerminalPayloadReportsRetainedWorkSeparatelyFromDelivery(t *testing.T) {
	failed := terminalPayload("failed", Result{
		WorkInProgressCommit: "cafebabe",
		WorkInProgressBranch: "wip/execution-1",
		WorkInProgressPushed: true,
		LogTail:              "--- FAIL: TestThing",
	}, nil)
	if failed["wipCommit"] != "cafebabe" || failed["wipBranch"] != "wip/execution-1" || failed["wipPushed"] != true {
		t.Fatalf("failed terminal payload = %#v", failed)
	}
	if failed["logTail"] != "--- FAIL: TestThing" {
		t.Fatalf("failed terminal payload lost its log tail: %#v", failed)
	}
	if _, delivered := failed["branch"]; delivered || failed["pushed"] != false {
		t.Fatalf("failed terminal payload reported delivery: %#v", failed)
	}

	local := terminalPayload("failed", Result{WorkInProgressCommit: "cafebabe"}, nil)
	if local["wipPushed"] != false {
		t.Fatalf("locally retained work should report wipPushed=false: %#v", local)
	}
	if _, named := local["wipBranch"]; named {
		t.Fatalf("locally retained work should not name a remote branch: %#v", local)
	}

	// The orchestrator rejects a payload with more than 32 fields
	// (runner_events.MAX_PAYLOAD_FIELDS), so terminal payload growth is bounded
	// on the receiving side as well as by the encoded byte limit.
	full := terminalPayload("blocked", Result{
		WorkInProgressCommit: "cafebabe", WorkInProgressBranch: "wip/execution-1", LogTail: "tail", Branch: "agent/issue-7/run-1",
		Raw: map[string]any{"status": "blocked"}, Summary: "needs a credential", RemainingWork: []string{"obtain DEPLOY_KEY"},
	}, executionUsage(time.Now(), Result{}))
	full["failureFingerprint"], full["error"], full["blocked"] = "fingerprint", "boom", true
	if len(full) > 24 {
		t.Fatalf("terminal payload carries %d fields, leaving too little headroom under the orchestrator's limit", len(full))
	}

	delivered := terminalPayload("completed", Result{Branch: "agent/issue-7/run-1", Pushed: true}, nil)
	if delivered["branch"] != "agent/issue-7/run-1" || delivered["pushed"] != true {
		t.Fatalf("completed terminal payload = %#v", delivered)
	}
	for _, key := range []string{"wipBranch", "wipCommit", "wipPushed", "logTail"} {
		if _, present := delivered[key]; present {
			t.Fatalf("completed terminal payload carried %q: %#v", key, delivered)
		}
	}
}

// TestMinimalTerminalPayloadKeepsRetainedWorkAndDropsTheLogTail keeps the
// pointer to a failed run's work alive on the fallback path while shedding the
// unbounded excerpt that fallback exists for.
func TestMinimalTerminalPayloadKeepsRetainedWorkAndDropsTheLogTail(t *testing.T) {
	reduced := minimalTerminalPayload(map[string]any{
		"status":    "failed",
		"wipBranch": "wip/execution-1",
		"wipCommit": "cafebabe",
		"wipPushed": true,
		"logTail":   strings.Repeat("z", 4096),
	})
	if reduced == nil {
		t.Fatal("minimalTerminalPayload() did not reduce a payload carrying a log tail")
	}
	if reduced["wipBranch"] != "wip/execution-1" || reduced["wipCommit"] != "cafebabe" || reduced["wipPushed"] != true {
		t.Fatalf("reduced payload lost the pointer to the retained work: %#v", reduced)
	}
	if _, present := reduced["logTail"]; present {
		t.Fatalf("reduced payload kept the unbounded log tail: %#v", reduced)
	}
}

func TestMinimalTerminalPayloadReducesOnlyWhenSomethingChanges(t *testing.T) {
	if reduced := minimalTerminalPayload(map[string]any{"status": "failed", "exitCode": 1}); reduced != nil {
		t.Fatalf("minimalTerminalPayload() reduced an already-minimal payload: %#v", reduced)
	}

	// Same key count as the whitelist, but a disjoint key set: a count
	// comparison would wrongly report nothing to reduce.
	disjoint := map[string]any{"changedFiles": []string{"a"}, "commandsRun": []string{"b"}, "committed": true, "pushed": false, "finalRevision": "abc", "result": map[string]any{}}
	reduced := minimalTerminalPayload(disjoint)
	if reduced == nil || len(reduced) != 0 {
		t.Fatalf("minimalTerminalPayload(disjoint) = %#v, want an empty reduction", reduced)
	}

	// Truncation alone is a reduction even when no key is dropped.
	long := strings.Repeat("y", maxTerminalPayloadFieldBytes+10)
	reduced = minimalTerminalPayload(map[string]any{"error": long})
	if reduced == nil {
		t.Fatal("minimalTerminalPayload() did not reduce an oversized error field")
	}
	text := reduced["error"].(string)
	if jsonEncodedSize(text) > maxTerminalPayloadFieldBytes || !contains(text, truncationMarker) {
		t.Fatalf("truncated error field = %d encoded bytes, marker present = %v", jsonEncodedSize(text), contains(text, truncationMarker))
	}
}

func TestBoundedAgentTextStopsOnARuneBoundary(t *testing.T) {
	value := strings.Repeat("a", 5) + "界界界"
	for budget := 5; budget <= jsonEncodedSize(value)+len(truncationMarker); budget++ {
		bounded := boundedAgentText(value, budget)
		if jsonEncodedSize(bounded) > budget && bounded != truncationMarker {
			t.Fatalf("boundedAgentText(%d) = %d encoded bytes", budget, jsonEncodedSize(bounded))
		}
		if !utf8.ValidString(bounded) {
			t.Fatalf("boundedAgentText(%d) split a rune: %q", budget, bounded)
		}
	}
	if bounded := boundedAgentText(value, 4096); bounded != value {
		t.Fatalf("boundedAgentText() altered text that fits: %q", bounded)
	}
}

// TestBoundedAgentTextSanitisesBeforeMeasuring: agent prose reaches the payload
// through the same hazards as a log tail — terminal escapes, control bytes, and
// invalid UTF-8 — and all three cost far more encoded than raw.
func TestBoundedAgentTextSanitisesBeforeMeasuring(t *testing.T) {
	bounded := boundedAgentText("\x1b[31mblocked\x1b[0m on \x00credentials\xff", maxTerminalPayloadFieldBytes)
	if bounded != "blocked on credentials" {
		t.Fatalf("boundedAgentText() = %q", bounded)
	}
	if !utf8.ValidString(bounded) {
		t.Fatalf("boundedAgentText() left invalid UTF-8: %q", bounded)
	}
}

// TestBoundedListDropsBlankEntries: an agent that writes `remainingWork: ["",
// ""]` has reported nothing, and empty strings on the wire only look like data.
func TestBoundedListDropsBlankEntries(t *testing.T) {
	if bounded := boundedList([]string{"", "   ", "\x1b[0m"}); bounded != nil {
		t.Fatalf("boundedList(blank) = %#v, want nil", bounded)
	}
	if bounded := boundedList(nil); bounded != nil {
		t.Fatalf("boundedList(nil) = %#v, want nil", bounded)
	}
	bounded := boundedList([]string{"", "obtain DEPLOY_KEY", "  "})
	if len(bounded) != 1 || bounded[0] != "obtain DEPLOY_KEY" {
		t.Fatalf("boundedList() = %#v", bounded)
	}
}

func findLogEntry(t *testing.T, output, message string) (map[string]any, bool) {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse structured log %q: %v", line, err)
		}
		if entry["msg"] == message {
			return entry, true
		}
	}
	return nil, false
}

func waitForPendingEvents(t *testing.T, loop *ControlLoop, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if loop.Reporter.Pending() == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending events = %d, want %d", loop.Reporter.Pending(), count)
}

func TestControlLoopWithCapacityRunsExecutionsForDifferentProjectsConcurrently(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{Status: "completed"}}
	loop, err := NewControlLoopWithCapacity(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "", 2)
	if err != nil {
		t.Fatalf("NewControlLoopWithCapacity() error = %v", err)
	}

	first := loopOfferFor(t, "job-1", "execution-1", "project-1")
	second := loopOfferFor(t, "job-2", "execution-2", "project-2")
	for _, offer := range []*runnerv1.JobOffer{first, second} {
		if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
			t.Fatalf("Handle(offer %s) error = %v", offer.GetJobId(), err)
		}
	}
	client.mu.Lock()
	if len(client.accepted) != 2 {
		t.Fatalf("accepted = %#v, want both offers accepted under capacity 2", client.accepted)
	}
	client.mu.Unlock()

	for _, offer := range []*runnerv1.JobOffer{first, second} {
		ack := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}
		if err := loop.Handle(context.Background(), ack); err != nil {
			t.Fatalf("Handle(acknowledgement %s) error = %v", offer.GetJobId(), err)
		}
	}
	waitForEvents(t, client, 4)

	dispatcher.mu.Lock()
	calls := dispatcher.calls
	dispatcher.mu.Unlock()
	if calls != 2 {
		t.Fatalf("dispatcher calls = %d, want 2 concurrent executions", calls)
	}
	if loop.Busy() {
		t.Fatal("loop reports busy after both leases were consumed to capacity and completed")
	}
}

// TestControlLoopReportsRecoverableWorkAfterAPipelineFailure is the end-to-end
// check for this behaviour: a run whose pipeline fails still commits what the
// agent produced, publishes it under a work-in-progress ref, and names that
// branch and commit in the terminal event the orchestrator receives — while the
// delivery branch stays untouched.
func TestControlLoopReportsRecoverableWorkAfterAPipelineFailure(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	retention := t.TempDir()
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "cafebabe"}}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            &backend{result: agents.Result{Status: "completed", Summary: "implemented"}},
		Delivery:           delivery,
		Environment:        environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}},
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
		Pipeline: pipelineRunner{
			results: []pipeline.Result{{Command: "go test ./...", ExitCode: 1, Output: "--- FAIL: TestThing\n"}},
			err:     errors.New("pipeline command failed with exit code 1: go test ./..."),
		},
		PushWorkInProgress: true,
		Retention:          RetentionPolicy{KeepFailed: true, Directory: retention, MaxAge: time.Hour, MaxWorkspaces: 4},
	}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}

	offer := credentialOffer(t)
	packet := taskpacket.Packet{}
	if err := json.Unmarshal([]byte(offer.GetTaskPacketJson()), &packet); err != nil {
		t.Fatal(err)
	}
	packet.Pipeline = []taskpacket.PipelineCommand{{Command: "go test ./...", TimeoutSeconds: 30}}
	contents, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	offer.TaskPacketJson = string(contents)

	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	acknowledgement := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}}}
	if err := loop.Handle(context.Background(), acknowledgement); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	waitForEvents(t, client, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	terminal := client.events[1]
	if terminal.GetType() != "failed" {
		t.Fatalf("terminal event = %#v, want failed", terminal)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(terminal.GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["wipBranch"] != "wip/execution-1" || payload["wipCommit"] != "cafebabe" || payload["wipPushed"] != true {
		t.Fatalf("terminal payload does not name the recoverable work: %#v", payload)
	}
	if payload["logTail"] != "--- FAIL: TestThing" {
		t.Fatalf("terminal payload log tail = %#v", payload["logTail"])
	}
	if _, delivered := payload["branch"]; delivered || payload["pushed"] != false {
		t.Fatalf("a pipeline-failed run reported delivery: %#v", payload)
	}
	if len(delivery.pushes) != 0 || len(delivery.workInProgressPushes) != 1 {
		t.Fatalf("delivery pushes = %#v, work-in-progress pushes = %#v", delivery.pushes, delivery.workInProgressPushes)
	}
	if delivery.workInProgressEnv["GITHUB_TOKEN"] != "token-value" {
		t.Fatalf("work-in-progress push environment = %#v", delivery.workInProgressEnv)
	}
	if manager.cleaned {
		t.Fatal("the failed workspace was cleaned up instead of retained")
	}
	records, err := filepath.Glob(filepath.Join(retention, "retained", "*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("retention records = %#v, %v", records, err)
	}
}
