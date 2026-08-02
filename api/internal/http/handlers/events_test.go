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

type synchronizedRecorder struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	wrote chan struct{}
}

func newSynchronizedRecorder() *synchronizedRecorder {
	return &synchronizedRecorder{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{}, 8)}
}

func (r *synchronizedRecorder) Write(data []byte) (int, error) {
	r.mu.Lock()
	n, err := r.ResponseRecorder.Write(data)
	r.mu.Unlock()
	r.wrote <- struct{}{}
	return n, err
}

func (r *synchronizedRecorder) Flush() {}

func (r *synchronizedRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

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
	handler.keepAliveInterval = time.Hour
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	rec := newSynchronizedRecorder()
	finished := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(finished)
	}()
	<-rec.wrote

	client.events <- &controlv1.ControlPlaneEvent{
		Id: "42", EventType: "workflow",
		Workflow: &controlv1.Workflow{Id: "wf-1", ProjectId: "p-1", Status: "implementing", Phase: "implementing"},
	}
	select {
	case <-rec.wrote:
	case <-time.After(time.Second):
		t.Fatal("workflow event was not written")
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
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type %q", got)
	}
	if !strings.Contains(rec.body(), `"status":"implementing"`) {
		t.Fatalf("workflow event missing: %q", rec.body())
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
	rec := newSynchronizedRecorder()
	finished := make(chan struct{})
	go func() { handler.stream(rec, req); close(finished) }()
	<-rec.wrote
	select {
	case <-rec.wrote:
	case <-time.After(time.Second):
		t.Fatal("keepalive was not written")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop")
	}
}
