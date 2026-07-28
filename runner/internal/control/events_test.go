package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/taskpacket"
	"google.golang.org/protobuf/proto"
)

type eventClient struct {
	events []*runnerv1.ExecutionEvent
	err    error
}

type blockingEventClient struct {
	started chan struct{}
	release chan struct{}
}

func (c *eventClient) SendExecutionEvent(event *runnerv1.ExecutionEvent) error {
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, proto.Clone(event).(*runnerv1.ExecutionEvent))
	return nil
}

func (c *blockingEventClient) SendExecutionEvent(*runnerv1.ExecutionEvent) error {
	close(c.started)
	<-c.release
	return nil
}

func TestEventReporterRedactsConfiguredSecretPrefixes(t *testing.T) {
	client := &eventClient{}
	reporter, err := NewEventReporter(client, 4, []string{"internal_"}, "")
	if err != nil {
		t.Fatal(err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "internal_abcdefghijklmnopqrstuvwxyz"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(client.events[0].PayloadJson, "internal_abcdefghijklmnopqrstuvwxyz") {
		t.Fatal("configured secret leaked")
	}
	if _, err := NewEventReporter(client, 4, []string{"bad\nprefix"}, ""); err == nil {
		t.Fatal("unsafe configured prefix was accepted")
	}
}

func TestEventReporterRedactsKnownSecretValuesWithinStrings(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{
		"message": "tokens ghp_abcdefghijklmnopqrstuvwxyz github_pat_abcdefghijklmnop sk-abcdefgh",
	}); err != nil {
		t.Fatal(err)
	}
	payload := client.events[0].PayloadJson
	for _, secret := range []string{"ghp_abcdefghijklmnopqrstuvwxyz", "github_pat_abcdefghijklmnop", "sk-abcdefgh"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("secret leaked in %s", payload)
		}
	}
	if strings.Count(payload, "[REDACTED]") != 3 {
		t.Fatalf("expected redactions in %s", payload)
	}
}

func TestEventReporterSplitsUTF8LogsIntoOrderedPayloads(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 8)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	message := string(make([]byte, maxLogChunkBytes-1)) + "界界"
	for index := range message[:maxLogChunkBytes-1] {
		message = message[:index] + "x" + message[index+1:]
	}
	sequences, err := reporter.EmitLog(lease.JobID, lease.Generation, message)
	if err != nil {
		t.Fatal(err)
	}
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 || len(client.events) != 2 {
		t.Fatalf("unexpected log sequences/events: %v/%d", sequences, len(client.events))
	}
	var first map[string]any
	var second map[string]any
	if err := json.Unmarshal([]byte(client.events[0].PayloadJson), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(client.events[1].PayloadJson), &second); err != nil {
		t.Fatal(err)
	}
	if first["chunkIndex"] != float64(0) || second["chunkIndex"] != float64(1) || first["chunkCount"] != float64(2) {
		t.Fatalf("unexpected chunk metadata: %#v %#v", first, second)
	}
	if first["message"].(string)+second["message"].(string) != message {
		t.Fatal("log chunks did not preserve message")
	}
}

func TestEventReporterSendsOrderedFencedRedactedEvents(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	for _, event := range []struct {
		typeName string
		payload  map[string]any
	}{
		{"started", map[string]any{"status": "preparing"}},
		{"log", map[string]any{"message": "working", "token": "not-safe", "nested": map[string]any{"password": "not-safe"}}},
		{"completed", map[string]any{"status": "completed"}},
	} {
		if _, err := reporter.Emit(lease.JobID, lease.Generation, event.typeName, event.payload); err != nil {
			t.Fatalf("Emit(%q) error = %v", event.typeName, err)
		}
	}
	if len(client.events) != 3 {
		t.Fatalf("sent events = %d", len(client.events))
	}
	for sequence, event := range client.events {
		if event.GetJobId() != lease.JobID || event.GetExecutionId() != lease.Packet.ExecutionID || event.GetLeaseGeneration() != lease.Generation || event.GetEventSequence() != int64(sequence+1) {
			t.Fatalf("event %d = %#v", sequence, event)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.events[1].GetPayloadJson()), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["token"] != "[REDACTED]" || payload["nested"].(map[string]any)["password"] != "[REDACTED]" {
		t.Fatalf("payload was not redacted: %#v", payload)
	}
}

func TestEventReporterAbandonsWhileTransportSendIsBlocked(t *testing.T) {
	client := &blockingEventClient{started: make(chan struct{}), release: make(chan struct{})}
	reporter := newEventReporter(t, client, 1)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil)
		finished <- err
	}()
	<-client.started
	abandoned := make(chan bool, 1)
	go func() { abandoned <- reporter.Abandon(lease.JobID, lease.Generation) }()
	select {
	case result := <-abandoned:
		if !result {
			t.Fatal("Abandon() did not clear the active lease")
		}
	case <-time.After(time.Second):
		t.Fatal("Abandon() blocked on transport send")
	}
	close(client.release)
	if err := <-finished; err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	if pending := reporter.Pending(); pending != 0 {
		t.Fatalf("pending events = %d, want 0", pending)
	}
}

func TestEventReporterBuffersAndReplaysAfterReconnect(t *testing.T) {
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter := newEventReporter(t, client, 3)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if sequence, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil); !errors.Is(err, disconnected) || sequence != 1 {
		t.Fatalf("first Emit() = (%d, %v)", sequence, err)
	}
	if sequence, err := reporter.Emit(lease.JobID, lease.Generation, "progress", nil); !errors.Is(err, disconnected) || sequence != 2 {
		t.Fatalf("second Emit() = (%d, %v)", sequence, err)
	}
	if reporter.Pending() != 2 {
		t.Fatalf("pending events = %d", reporter.Pending())
	}
	client.err = nil
	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if reporter.Pending() != 0 || len(client.events) != 2 || client.events[0].GetEventSequence() != 1 || client.events[1].GetEventSequence() != 2 {
		t.Fatalf("replayed events = %#v, pending = %d", client.events, reporter.Pending())
	}
}

func TestEventReporterRejectsStaleLeaseAndBoundsPendingEvents(t *testing.T) {
	client := &eventClient{err: errors.New("disconnected")}
	reporter := newEventReporter(t, client, 1)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation+1, "started", nil); !errors.Is(err, ErrStaleEventLease) {
		t.Fatalf("Emit() error = %v, want stale lease", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil); err == nil {
		t.Fatal("Emit() unexpectedly succeeded while disconnected")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "progress", nil); err == nil {
		t.Fatal("Emit() exceeded bounded buffer")
	}
	if !reporter.Abandon(lease.JobID, lease.Generation) {
		t.Fatal("Abandon() did not remove matching lease")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "cancelled", nil); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("Emit() after abandonment = %v", err)
	}
}

func TestEventReporterRejectsUnsafePayloads(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 1)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "unknown", nil); err == nil {
		t.Fatal("Emit() accepted an unknown event type")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": string(make([]byte, maxExecutionEventPayloadBytes))}); err == nil {
		t.Fatal("Emit() accepted an oversized payload")
	}
}

func TestEventReporterPersistsTerminalEventsForStartupReplay(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "outbox", "events.json")
	lease := eventLease()
	disconnected := errors.New("disconnected")
	initialClient := &eventClient{err: disconnected}
	reporter, err := NewEventReporter(initialClient, 4, nil, outboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"}); !errors.Is(err, disconnected) {
		t.Fatalf("Emit() error = %v", err)
	}
	if !reporter.Finish(lease.JobID, lease.Generation) {
		t.Fatal("Finish() did not release the event lease")
	}
	info, err := os.Stat(outboxPath)
	if err != nil {
		t.Fatalf("event outbox missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("event outbox permissions = %o", info.Mode().Perm())
	}
	replayClient := &eventClient{}
	replayed, err := NewEventReporter(replayClient, 4, nil, outboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Pending() != 1 {
		t.Fatalf("replayed pending events = %d", replayed.Pending())
	}
	if err := replayed.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if len(replayClient.events) != 1 || replayClient.events[0].GetType() != "completed" || replayClient.events[0].GetEventSequence() != 1 {
		t.Fatalf("replayed events = %#v", replayClient.events)
	}
	if _, err := os.Stat(outboxPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("event outbox still exists: %v", err)
	}
}

func TestEventReporterRejectsCorruptPersistentOutbox(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(outboxPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEventReporter(&eventClient{}, 4, nil, outboxPath); err == nil {
		t.Fatal("NewEventReporter() accepted corrupt outbox")
	}
}

func newEventReporter(t *testing.T, client EventClient, maxPending int) *EventReporter {
	t.Helper()
	reporter, err := NewEventReporter(client, maxPending, nil, "")
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	return reporter
}

func eventLease() Lease {
	return Lease{JobID: "job-1", Generation: 2, Packet: taskpacket.Packet{JobID: "job-1", ExecutionID: "execution-1"}}
}
