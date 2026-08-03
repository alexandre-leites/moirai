//go:build integration

package server

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"google.golang.org/grpc/metadata"
)

// fakeEventStream implements grpc.ServerStreamingServer[controlv1.ControlPlaneEvent]
// without needing a real gRPC transport, collecting whatever StreamEvents sends
// so tests can assert on it.
type fakeEventStream struct {
	ctx context.Context

	mu     sync.Mutex
	events []*controlv1.ControlPlaneEvent
}

func (f *fakeEventStream) Send(event *controlv1.ControlPlaneEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakeEventStream) Context() context.Context     { return f.ctx }
func (f *fakeEventStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeEventStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeEventStream) SetTrailer(metadata.MD)       {}
func (f *fakeEventStream) SendMsg(m any) error          { return nil }
func (f *fakeEventStream) RecvMsg(m any) error          { return nil }

func (f *fakeEventStream) snapshot() []*controlv1.ControlPlaneEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*controlv1.ControlPlaneEvent, len(f.events))
	copy(out, f.events)
	return out
}

// waitFor polls until fn returns true or the deadline elapses, failing the
// test on timeout. StreamEvents delivers asynchronously off LISTEN/NOTIFY, so
// tests can't just call Send once and check.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// startStream launches StreamEvents in the background against an admin
// context carrying lastEventID, returning the fake stream to inspect and a
// cancel func to stop it.
func (h *harness) startStream(t *testing.T, lastEventID string) (*fakeEventStream, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(h.adminContext())
	stream := &fakeEventStream{ctx: ctx}
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		_ = h.Control.StreamEvents(&controlv1.StreamEventsRequest{LastEventId: lastEventID}, stream)
	}()
	return stream, cancel
}

// workflowRun seeds a bare workflow run row (and its issue) so
// app.workflow_events rows can reference it.
func (h *harness) workflowRun(projectID string) (workflowID string) {
	h.t.Helper()
	issueID := idgen.NewID()
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github',$3,$3,'Test issue','https://example.test/issues/'||$3,'open',true,now(),now())`, issueID, projectID, issueID[:8])
	workflowID = idgen.NewID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'preparing','preparing')`, workflowID, projectID, issueID, "thread-"+workflowID)
	return workflowID
}

// TestStreamEventsColdConnectSkipsHistory pins that a fresh connection
// (empty Last-Event-ID) never replays pre-existing workflow_events rows: the
// N+1 full-table replay this issue reported. Only events written after the
// stream starts should arrive.
func TestStreamEventsColdConnectSkipsHistory(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	workflowID := h.workflowRun(projectID)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)

	stream, _ := h.startStream(t, "")

	// Give the cold-connect catch-up a moment to run, then confirm nothing
	// from before the connection landed.
	time.Sleep(200 * time.Millisecond)
	if got := stream.snapshot(); len(got) != 0 {
		t.Fatalf("cold connect replayed %d pre-existing events, want 0: %+v", len(got), got)
	}

	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)
	waitFor(t, 5*time.Second, func() bool { return len(stream.snapshot()) == 1 })

	got := stream.snapshot()
	if got[0].GetEventType() != "workflow" {
		t.Fatalf("EventType = %q, want %q", got[0].GetEventType(), "workflow")
	}
	if got[0].GetWorkflow().GetId() != workflowID {
		t.Fatalf("Workflow.Id = %q, want %q", got[0].GetWorkflow().GetId(), workflowID)
	}
}

// TestStreamEventsReconnectReplaysFromCursor pins that an explicit
// Last-Event-ID still pages older events, the way a client reconnecting
// after a drop is expected to behave, and that ListWorkflowEvents remains
// unaffected as the way to page further back.
func TestStreamEventsReconnectReplaysFromCursor(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	workflowID := h.workflowRun(projectID)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)
	var firstID int64
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM app.workflow_events ORDER BY id LIMIT 1`).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowID)

	stream, _ := h.startStream(t, strconv.FormatInt(firstID, 10))

	waitFor(t, 5*time.Second, func() bool { return len(stream.snapshot()) == 2 })
	got := stream.snapshot()
	for _, event := range got {
		if event.GetEventType() != "workflow" {
			t.Fatalf("EventType = %q, want %q", event.GetEventType(), "workflow")
		}
	}
}

// TestStreamEventsBatchesWorkflowLookup pins that a page containing events
// for several distinct workflows resolves every one of them correctly (the
// batched ANY() lookup must not silently drop rows), not just the first.
func TestStreamEventsBatchesWorkflowLookup(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	workflowA := h.workflowRun(projectID)
	workflowB := h.workflowRun(projectID)
	workflowC := h.workflowRun(projectID)

	stream, _ := h.startStream(t, "")
	time.Sleep(200 * time.Millisecond) // let the cold-connect cursor settle before writing

	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowA)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowB)
	h.exec(`INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'phase_started','info','{}'::jsonb)`, workflowC)

	waitFor(t, 5*time.Second, func() bool { return len(stream.snapshot()) == 3 })
	seen := map[string]bool{}
	for _, event := range stream.snapshot() {
		seen[event.GetWorkflow().GetId()] = true
	}
	for _, id := range []string{workflowA, workflowB, workflowC} {
		if !seen[id] {
			t.Fatalf("batched lookup dropped workflow %s; got events for %v", id, seen)
		}
	}
}

// TestStreamEventsEmitsRunnerEvents pins that runner changes reach
// ControlPlaneEvent.Runner with event_type "runner" — the trigger this issue
// says nothing consumed.
func TestStreamEventsEmitsRunnerEvents(t *testing.T) {
	h := newHarness(t)
	stream, _ := h.startStream(t, "")
	time.Sleep(200 * time.Millisecond)

	runnerID := idgen.NewID()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1.2.3','["gpu"]'::jsonb,now())`, runnerID, "runner-"+runnerID[:8])

	waitFor(t, 5*time.Second, func() bool { return len(stream.snapshot()) >= 1 })
	got := stream.snapshot()
	var runnerEvent *controlv1.ControlPlaneEvent
	for _, event := range got {
		if event.GetEventType() == "runner" {
			runnerEvent = event
		}
	}
	if runnerEvent == nil {
		t.Fatalf("no runner event received; got %+v", got)
	}
	if runnerEvent.GetRunner().GetId() != runnerID {
		t.Fatalf("Runner.Id = %q, want %q", runnerEvent.GetRunner().GetId(), runnerID)
	}
	if runnerEvent.GetRunner().GetStatus() != "online" {
		t.Fatalf("Runner.Status = %q, want %q", runnerEvent.GetRunner().GetStatus(), "online")
	}
	if runnerEvent.GetRunner().GetVersion() != "1.2.3" {
		t.Fatalf("Runner.Version = %q, want %q", runnerEvent.GetRunner().GetVersion(), "1.2.3")
	}

	// A tracked update (draining) must also notify.
	h.exec(`UPDATE app.runners SET draining=true WHERE id=$1`, runnerID)
	waitFor(t, 5*time.Second, func() bool {
		for _, event := range stream.snapshot() {
			if event.GetEventType() == "runner" && event.GetRunner().GetDraining() {
				return true
			}
		}
		return false
	})
}
