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

// deadlineTrackingRecorder augments synchronizedRecorder with the
// SetWriteDeadline method net/http.ResponseController looks for (see
// https://pkg.go.dev/net/http#ResponseController.SetWriteDeadline). httptest's
// ResponseRecorder does not implement it, and a real *http.Server response
// writer does not surface it under go test either, so this fake is what lets
// the test observe whether the handler actually asks for the write deadline
// to be cleared — the exact regression the SSE stream needs to be safe from
// an http.Server.WriteTimeout (see server.go's WriteTimeout) killing a
// connection open longer than that timeout.
type deadlineTrackingRecorder struct {
	*synchronizedRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func newDeadlineTrackingRecorder() *deadlineTrackingRecorder {
	return &deadlineTrackingRecorder{synchronizedRecorder: newSynchronizedRecorder()}
}

func (r *deadlineTrackingRecorder) SetWriteDeadline(deadline time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func (r *deadlineTrackingRecorder) calls() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.deadlines...)
}

// TestEventStreamClearsWriteDeadline guards against the SSE stream being
// silently killed by http.Server.WriteTimeout: under HTTP/1.1 that deadline
// covers the whole response body, not just the time to write one chunk of
// it, so a long-lived stream that never clears it gets disconnected at the
// timeout even though nothing is wrong. httptest.NewServer sets no
// WriteTimeout, so a test built on it can't reproduce the failure directly;
// asserting the handler calls SetWriteDeadline(time.Time{}) verifies the fix
// without needing to actually run a connection past the timeout.
func TestEventStreamClearsWriteDeadline(t *testing.T) {
	client := &fakeEventClient{events: make(chan *controlv1.ControlPlaneEvent)}
	handler := NewEventHandlers(client)
	handler.keepAliveInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	req = req.WithContext(auth.WithSessionToken(req.Context(), "session"))
	rec := newDeadlineTrackingRecorder()
	finished := make(chan struct{})
	go func() { handler.stream(rec, req); close(finished) }()
	<-rec.wrote
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("handler did not stop")
	}

	calls := rec.calls()
	if len(calls) == 0 {
		t.Fatal("SetWriteDeadline was never called; the http.Server.WriteTimeout will kill this stream at 60s")
	}
	found := false
	for _, deadline := range calls {
		if deadline.IsZero() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SetWriteDeadline was not called with a zero time to clear the deadline, got %v", calls)
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
		Workflow: &controlv1.Workflow{Id: "wf-1", ProjectId: "p-1", Status: "preparing", Phase: "preparing"},
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
	if !strings.Contains(rec.body(), `"status":"preparing"`) {
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
