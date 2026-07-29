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

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
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
	dispatcher := &staticDispatcher{result: Result{Status: "completed", ExitCode: 0, Summary: "token should not leave runner", ChangedFiles: []string{"main.go"}, CommandsRun: []string{"go test ./..."}}}
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
	if client.events[1].GetPayloadJson() == "" || contains(client.events[1].GetPayloadJson(), "token should not leave runner") {
		t.Fatalf("terminal payload leaked result summary: %s", client.events[1].GetPayloadJson())
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.events[1].GetPayloadJson()), &payload); err != nil {
		t.Fatalf("parse terminal payload: %v", err)
	}
	if payload["durationMs"] == nil || payload["changedFileCount"] != float64(1) || payload["commandCount"] != float64(1) || payload["pipelineCommandCount"] != float64(0) {
		t.Fatalf("terminal usage payload = %#v", payload)
	}
}

func TestControlLoopReportsFailureWithoutStartingRenewalAcknowledgementTwice(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &staticDispatcher{result: Result{ExitCode: 1, Summary: "credential=unsafe"}, err: context.DeadlineExceeded}
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
	if len(client.events) != 2 || client.events[1].GetType() != "failed" || contains(client.events[1].GetPayloadJson(), "unsafe") {
		t.Fatalf("failure events = %#v", client.events)
	}
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

func TestTerminalPayloadCarriesRawResultOnlyWhenCompleted(t *testing.T) {
	raw := map[string]any{"verdict": "approved", "findings": []any{}}
	completed := terminalPayload("completed", Result{Raw: raw}, nil)
	result, ok := completed["result"].(map[string]any)
	if !ok || result["verdict"] != "approved" {
		t.Fatalf("terminalPayload(completed) result = %#v", completed["result"])
	}

	failed := terminalPayload("failed", Result{Raw: raw}, nil)
	if _, present := failed["result"]; present {
		t.Fatalf("terminalPayload(failed) should not carry result: %#v", failed)
	}

	noRaw := terminalPayload("completed", Result{}, nil)
	if _, present := noRaw["result"]; present {
		t.Fatalf("terminalPayload(completed) with no raw document should omit result: %#v", noRaw)
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
