package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

const eventKeepAliveInterval = 15 * time.Second

type eventClient interface {
	StreamEvents(context.Context, string) (orchestrator.EventStream, error)
}

type EventHandlers struct {
	client            eventClient
	keepAliveInterval time.Duration
}

func NewEventHandlers(client eventClient) *EventHandlers {
	return &EventHandlers{client: client, keepAliveInterval: eventKeepAliveInterval}
}

func (h *EventHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/events", auth.RequireSession(http.HandlerFunc(h.stream)))
}

func (h *EventHandlers) stream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(requestContext(r))
	defer cancel()

	stream, err := h.client.StreamEvents(ctx, r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeClientError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	rc := http.NewResponseController(w)
	// The server sets an http.Server.WriteTimeout so that ordinary handlers
	// cannot hang a connection forever, but under HTTP/1.1 that deadline
	// applies to the whole response body, not just the time to write it —
	// so it would silently kill this long-lived stream at the timeout
	// (currently 60s) even though the client and orchestrator are both
	// healthy. Clearing it here is safe: the request context (cancelled on
	// client disconnect, see the r.Context().Done() case below) remains the
	// mechanism that ends an idle or abandoned connection. Some
	// ResponseWriters (e.g. httptest's) don't support deadlines at all and
	// return http.ErrNotSupported, which is fine to ignore here.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}

	if err := writeSSEComment(w, "connected"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	done := make(chan struct{})
	defer close(done)
	received := make(chan eventResult, 1)
	go func() {
		for {
			event, err := stream.Recv()
			select {
			case received <- eventResult{event: event, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(h.keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := writeSSEComment(w, "keepalive"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case result := <-received:
			if errors.Is(result.err, io.EOF) || errors.Is(result.err, context.Canceled) {
				return
			}
			if result.err != nil {
				return
			}
			if err := writeSSEEvent(w, result.event); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

type eventResult struct {
	event *controlv1.ControlPlaneEvent
	err   error
}

func writeSSEComment(w http.ResponseWriter, comment string) error {
	_, err := fmt.Fprintf(w, ": %s\n\n", comment)
	return err
}

func writeSSEEvent(w http.ResponseWriter, event *controlv1.ControlPlaneEvent) error {
	payload := map[string]any{"type": event.GetEventType()}
	if workflow := event.GetWorkflow(); workflow != nil {
		payload["workflow"] = workflowPayload(workflow)
	}
	if runner := event.GetRunner(); runner != nil {
		payload["runner"] = runnerPayload(runner)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\nretry: 1000\ndata: %s\n\n", event.GetId(), event.GetEventType(), data)
	return err
}
