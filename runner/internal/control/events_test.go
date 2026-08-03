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

// TestEventReporterRedactsNativeStringLists covers the shape terminal payloads
// actually use: they are built in Go, so their string lists are []string and
// never reach the []any arm of the redactor.
func TestEventReporterRedactsNativeStringLists(t *testing.T) {
	client := &eventClient{}
	reporter, err := NewEventReporter(client, 4, []string{"internal_"}, "")
	if err != nil {
		t.Fatal(err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{
		"remainingWork": []string{"rotate internal_abcdefghijklmnop", "rotate ghp_abcdefghijklmnop"},
		"commandsRun":   []string{"deploy --key sk-abcdefgh"},
	}); err != nil {
		t.Fatal(err)
	}
	payload := client.events[0].PayloadJson
	for _, secret := range []string{"internal_abcdefghijklmnop", "ghp_abcdefghijklmnop", "sk-abcdefgh"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("secret leaked from a string list in %s", payload)
		}
	}
	if !strings.Contains(payload, "rotate ") {
		t.Fatalf("redaction destroyed the surrounding text in %s", payload)
	}
}

// TestEventReporterLeavesOrdinaryPathsAndCommandsIntact: a secret prefix that
// continues an identifier is part of a longer word, not a credential. `sk-`
// occurs inside `task-`, `disk-`, and `risk-`, and terminal payloads now carry
// the changed-file and command lists those appear in.
func TestEventReporterLeavesOrdinaryPathsAndCommandsIntact(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{
		"changedFiles": []string{"internal/task-queue/main.go", "docs/risk-register.md", "src/disk-usage.ts"},
		"commandsRun":  []string{"make task-build"},
		"error":        "scripts/task-runner.py exited 1",
	}); err != nil {
		t.Fatal(err)
	}
	payload := client.events[0].PayloadJson
	if strings.Contains(payload, "[REDACTED]") {
		t.Fatalf("ordinary paths and commands were corrupted by redaction: %s", payload)
	}
	for _, kept := range []string{"internal/task-queue/main.go", "docs/risk-register.md", "src/disk-usage.ts", "make task-build", "scripts/task-runner.py"} {
		if !strings.Contains(payload, kept) {
			t.Fatalf("redaction mangled %q in %s", kept, payload)
		}
	}
}

// TestEventReporterStillRedactsSecretsAtTokenBoundaries pins the other side of
// the boundary rule: a credential preceded by punctuation is still a credential.
func TestEventReporterStillRedactsSecretsAtTokenBoundaries(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "failed", map[string]any{
		"error": `GITHUB_TOKEN=ghp_abcdefghijklmnop curl -H "Authorization: Bearer sk-abcdefgh" (glpat-zyxwvutsrq)`,
	}); err != nil {
		t.Fatal(err)
	}
	payload := client.events[0].PayloadJson
	for _, secret := range []string{"ghp_abcdefghijklmnop", "sk-abcdefgh", "glpat-zyxwvutsrq"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("secret at a token boundary leaked in %s", payload)
		}
	}
	if strings.Count(payload, "[REDACTED]") != 3 {
		t.Fatalf("expected three redactions in %s", payload)
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
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "progress", nil); !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("Emit() exceeded bounded buffer: %v", err)
	}
	if !reporter.Abandon(lease.JobID, lease.Generation) {
		t.Fatal("Abandon() did not remove matching lease")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "progress", nil); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("Emit(progress) after abandonment = %v", err)
	}
	if !reporter.Finish(lease.JobID, lease.Generation) {
		t.Fatal("Finish() did not release the expired lease record")
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

func TestEventReporterQuarantinesCorruptPersistentOutbox(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "events.json")
	if err := os.WriteFile(outboxPath, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEventReporter(&eventClient{}, 4, nil, outboxPath); err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	if _, err := os.Stat(outboxPath + ".corrupt"); err != nil {
		t.Fatalf("quarantined outbox = %v", err)
	}
}

func TestEventReporterKeepsTerminalEventsWhenLeaseExpires(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "outbox", "events.json")
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter, err := NewEventReporter(client, 8, nil, outboxPath)
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil); !errors.Is(err, disconnected) {
		t.Fatalf("Emit(started) error = %v", err)
	}
	if _, err := reporter.EmitLog(lease.JobID, lease.Generation, "chatty agent output"); !errors.Is(err, disconnected) {
		t.Fatalf("EmitLog() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"}); !errors.Is(err, disconnected) {
		t.Fatalf("Emit(completed) error = %v", err)
	}
	if !reporter.Abandon(lease.JobID, lease.Generation) {
		t.Fatal("Abandon() did not release the expired lease")
	}
	if pending := reporter.Pending(); pending != 1 {
		t.Fatalf("pending events after lease expiry = %d, want 1 terminal event", pending)
	}
	stored := loadOutbox(t, outboxPath)
	if len(stored) != 1 || stored[0].GetType() != "completed" {
		t.Fatalf("outbox after lease expiry = %#v, want the terminal event", stored)
	}
	client.err = nil
	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if len(client.events) != 1 || client.events[0].GetType() != "completed" {
		t.Fatalf("delivered events = %#v, want the terminal event", client.events)
	}
}

func TestEventReporterQueuesTerminalEventEmittedAfterLeaseExpiry(t *testing.T) {
	outboxPath := filepath.Join(t.TempDir(), "events.json")
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter, err := NewEventReporter(client, 8, nil, outboxPath)
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if !reporter.Abandon(lease.JobID, lease.Generation) {
		t.Fatal("Abandon() did not release the expired lease")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "late"}); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("Emit(log) after expiry = %v, want no active lease", err)
	}
	sequence, err := reporter.Emit(lease.JobID, lease.Generation, "failed", map[string]any{"status": "failed"})
	if sequence < 1 || !errors.Is(err, disconnected) {
		t.Fatalf("Emit(failed) after expiry = (%d, %v), want a queued sequence and the transport error", sequence, err)
	}
	stored := loadOutbox(t, outboxPath)
	if len(stored) != 1 || stored[0].GetType() != "failed" || stored[0].GetLeaseGeneration() != lease.Generation {
		t.Fatalf("outbox = %#v, want the fenced terminal event", stored)
	}
	client.err = nil
	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if len(client.events) != 1 || client.events[0].GetType() != "failed" {
		t.Fatalf("delivered events = %#v", client.events)
	}
	if !reporter.Finish(lease.JobID, lease.Generation) {
		t.Fatal("Finish() did not release the expired lease record")
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", nil); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("Emit() after Finish() = %v, want no active lease", err)
	}
}

func TestEventReporterEvictsDroppableEventsForTerminalEvent(t *testing.T) {
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter := newEventReporter(t, client, 3)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil); !errors.Is(err, disconnected) {
		t.Fatalf("Emit(started) error = %v", err)
	}
	for index := 0; index < 2; index++ {
		if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "chatty"}); !errors.Is(err, disconnected) {
			t.Fatalf("Emit(log %d) error = %v", index, err)
		}
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "overflow"}); !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("Emit(log) on a full buffer = %v, want a full-buffer rejection", err)
	}
	sequence, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"})
	if sequence != 4 || !errors.Is(err, disconnected) {
		t.Fatalf("Emit(completed) on a full buffer = (%d, %v), want the terminal event queued", sequence, err)
	}
	client.err = nil
	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if len(client.events) != 3 {
		t.Fatalf("delivered events = %#v, want three", client.events)
	}
	types := []string{client.events[0].GetType(), client.events[1].GetType(), client.events[2].GetType()}
	sequences := []int64{client.events[0].GetEventSequence(), client.events[1].GetEventSequence(), client.events[2].GetEventSequence()}
	if types[0] != "started" || types[1] != "log" || types[2] != "completed" {
		t.Fatalf("delivered types = %#v, want the oldest log evicted", types)
	}
	if sequences[0] != 1 || sequences[1] != 3 || sequences[2] != 4 {
		t.Fatalf("delivered sequences = %#v", sequences)
	}
}

func TestEventReporterRejectsTerminalEventWhenBufferHoldsOnlyTerminalEvents(t *testing.T) {
	client := &eventClient{err: errors.New("disconnected")}
	reporter := newEventReporter(t, client, 1)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", nil); err == nil {
		t.Fatal("Emit(completed) unexpectedly succeeded while disconnected")
	}
	sequence, err := reporter.Emit(lease.JobID, lease.Generation, "failed", nil)
	if sequence != 0 || !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("Emit(failed) = (%d, %v), want a full-buffer rejection", sequence, err)
	}
}

func TestEventReporterRetainsEveryExpiredGenerationOfAJob(t *testing.T) {
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter := newEventReporter(t, client, 8)
	first := eventLease()
	if err := reporter.Begin(first); err != nil {
		t.Fatalf("Begin(gen %d) error = %v", first.Generation, err)
	}
	if !reporter.Abandon(first.JobID, first.Generation) {
		t.Fatal("Abandon() did not release the first lease")
	}
	second := first
	second.Generation = first.Generation + 1
	if err := reporter.Begin(second); err != nil {
		t.Fatalf("Begin(gen %d) error = %v", second.Generation, err)
	}
	if !reporter.Abandon(second.JobID, second.Generation) {
		t.Fatal("Abandon() did not release the re-offered lease")
	}
	for _, lease := range []Lease{first, second} {
		sequence, err := reporter.Emit(lease.JobID, lease.Generation, "failed", map[string]any{"status": "failed"})
		if sequence < 1 || !errors.Is(err, disconnected) {
			t.Fatalf("Emit(failed, gen %d) = (%d, %v), want the terminal event queued", lease.Generation, sequence, err)
		}
	}
	if pending := reporter.Pending(); pending != 2 {
		t.Fatalf("pending events = %d, want one terminal event per expired generation", pending)
	}
	for _, lease := range []Lease{first, second} {
		if !reporter.Finish(lease.JobID, lease.Generation) {
			t.Fatalf("Finish(gen %d) did not release the retained lease", lease.Generation)
		}
	}
}

func TestEventReporterRestoresEvictedEventWhenPersistFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	outboxDirectory := t.TempDir()
	outboxPath := filepath.Join(outboxDirectory, "events.json")
	disconnected := errors.New("disconnected")
	client := &eventClient{err: disconnected}
	reporter, err := NewEventReporter(client, 2, nil, outboxPath)
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	for index := 0; index < 2; index++ {
		if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "chatty"}); !errors.Is(err, disconnected) {
			t.Fatalf("Emit(log %d) error = %v", index, err)
		}
	}
	// Make the outbox directory unwritable so the terminal Emit fails to persist
	// after it has already evicted a log event to make room for itself.
	if err := os.Chmod(outboxDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outboxDirectory, 0o700) })
	if sequence, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"}); sequence != 0 || err == nil {
		t.Fatalf("Emit(completed) = (%d, %v), want a persist failure", sequence, err)
	}
	if pending := reporter.Pending(); pending != 2 {
		t.Fatalf("pending events = %d, want the evicted log restored after the failed emit", pending)
	}
}

func TestEventReporterChunksLogsByEncodedPayloadLimit(t *testing.T) {
	client := &eventClient{}
	reporter, err := NewEventReporterWithLimits(client, 32, nil, "", 128, 1024)
	if err != nil {
		t.Fatal(err)
	}
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := reporter.EmitLog(lease.JobID, lease.Generation, strings.Repeat("<", 300)); err != nil {
		t.Fatal(err)
	}
	if len(client.events) < 2 {
		t.Fatalf("events = %d, want chunks", len(client.events))
	}
	for _, event := range client.events {
		if len(event.GetPayloadJson()) > 128 {
			t.Fatalf("payload = %d bytes", len(event.GetPayloadJson()))
		}
	}
}

func loadOutbox(t *testing.T, path string) []*runnerv1.ExecutionEvent {
	t.Helper()
	outbox, err := newEventOutbox(path)
	if err != nil {
		t.Fatalf("newEventOutbox() error = %v", err)
	}
	events, err := outbox.Load()
	if err != nil {
		t.Fatalf("outbox.Load() error = %v", err)
	}
	return events
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

// A provider key matches none of the token prefixes the redactor knows, which
// is why the redaction set is fed by the values actually resolved for a job.
// Issue #230: a run against a paid model must complete with the key never
// appearing in a log.
func TestEventReporterRedactsAJobsResolvedCredentialValues(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	// The key is registered before anything can be emitted for the job, which
	// is the ordering the dispatcher enforces: resolve, register, then run.
	reporter.RedactSecrets(lease.JobID, []string{"or-v1-9f2c1d4e8a7b6c5d"})

	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{
		"message": "provider replied 401 for key or-v1-9f2c1d4e8a7b6c5d",
	}); err != nil {
		t.Fatal(err)
	}

	payload := client.events[0].PayloadJson
	if strings.Contains(payload, "or-v1-9f2c1d4e8a7b6c5d") {
		t.Fatalf("provider key leaked in %s", payload)
	}
	if !strings.Contains(payload, "[REDACTED]") {
		t.Fatalf("expected a redaction in %s", payload)
	}
}

// Release happens in Finish rather than when the execution returns, because the
// terminal event carrying the agent's own summary is built after that.
func TestAJobsCredentialsStayRedactedUntilTheJobIsFinished(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	reporter.RedactSecrets(lease.JobID, []string{"or-v1-9f2c1d4e8a7b6c5d"})

	if _, err := reporter.Emit(lease.JobID, lease.Generation, "failed", map[string]any{
		"error": "agent exited: bad key or-v1-9f2c1d4e8a7b6c5d",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(client.events[0].PayloadJson, "or-v1-9f2c1d4e8a7b6c5d") {
		t.Fatalf("provider key leaked in the terminal payload %s", client.events[0].PayloadJson)
	}

	// Finish is the end of the job on every path, so the set does not grow
	// without bound across a long-lived runner.
	if !reporter.Finish(lease.JobID, lease.Generation) {
		t.Fatal("Finish() did not report the lease closed")
	}
	prefixes, literals := reporter.redaction.snapshot()
	if len(literals) != 0 {
		t.Fatalf("literals = %#v, want none after Finish", literals)
	}
	if len(prefixes) != len(reporter.redaction.prefixes) {
		t.Fatalf("configured prefixes were disturbed: %#v", prefixes)
	}
}

// Two executions may resolve the same deployment credential; the first to
// finish must not un-redact it for the other.
func TestARedactedValueSurvivesAnotherJobFinishing(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	reporter.RedactSecrets("job-a", []string{"or-v1-9f2c1d4e8a7b6c5d"})
	reporter.RedactSecrets("job-b", []string{"or-v1-9f2c1d4e8a7b6c5d"})

	reporter.redaction.release("job-a")

	_, literals := reporter.redaction.snapshot()
	if len(literals) != 1 || literals[0] != "or-v1-9f2c1d4e8a7b6c5d" {
		t.Fatalf("literals = %#v, want the value still held by job-b", literals)
	}
}

// Redacting "1" or "true" would blank most of a log and hide nothing.
func TestAValueTooShortToBeACredentialIsNotRedacted(t *testing.T) {
	client := &eventClient{}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}
	reporter.RedactSecrets(lease.JobID, []string{"true"})

	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{
		"message": "the gate verdict was true",
	}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(client.events[0].PayloadJson, "true") {
		t.Fatalf("an ordinary word was redacted: %s", client.events[0].PayloadJson)
	}
}
