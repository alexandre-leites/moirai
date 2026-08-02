package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

type fakeEventClient struct {
	events chan *controlv1.ControlPlaneEvent
	ctx    context.Context
	mu     sync.Mutex
}

func (c *fakeEventClient) StreamEvents(ctx context.Context, _ string) (orchestrator.EventStream, error) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
	return fakeEventStream{ctx: ctx, events: c.events}, nil
}

func (c *fakeEventClient) context() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctx
}

type fakeEventStream struct {
	ctx    context.Context
	events <-chan *controlv1.ControlPlaneEvent
}

func (s fakeEventStream) Recv() (*controlv1.ControlPlaneEvent, error) {
	select {
	case event := <-s.events:
		return event, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func TestEventRouteRequiresSession(t *testing.T) {
	mux := http.NewServeMux()
	NewEventHandlers(&fakeEventClient{events: make(chan *controlv1.ControlPlaneEvent)}).RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestEventStreamDeliversWorkflowAndTearsDown(t *testing.T) {
	client := &fakeEventClient{events: make(chan *controlv1.ControlPlaneEvent, 1)}
	handler := NewEventHandlers(client)
	handler.keepAliveInterval = time.Millisecond
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	rec := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(finished)
	}()

	client.events <- &controlv1.ControlPlaneEvent{
		Id: "42", EventType: "workflow",
		Workflow: &controlv1.Workflow{Id: "wf-1", ProjectId: "p-1", Status: "implementing", Phase: "implementing"},
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(rec.Body.String(), "id: 42") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	if !strings.Contains(rec.Body.String(), `"status":"implementing"`) {
		t.Fatalf("workflow event missing: %q", rec.Body.String())
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop after disconnect")
	}
	select {
	case <-client.context().Done():
	case <-time.After(time.Second):
		t.Fatal("orchestrator stream context was not cancelled")
	}
}

func TestEventStreamKeepAlive(t *testing.T) {
	client := &fakeEventClient{events: make(chan *controlv1.ControlPlaneEvent)}
	handler := NewEventHandlers(client)
	handler.keepAliveInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req = req.WithContext(auth.WithSessionToken(req.Context(), "session"))
	rec := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() { handler.stream(rec, req); close(finished) }()
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(rec.Body.String(), ": keepalive") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatal("keepalive was not written")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop")
	}
}
