package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type EventHandlers struct {
	client *orchestrator.Client
}

func NewEventHandlers(client *orchestrator.Client) *EventHandlers {
	return &EventHandlers{client: client}
}

func (h *EventHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/events", auth.RequireSession(http.HandlerFunc(h.subscribe)))
}

func (h *EventHandlers) subscribe(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeClientError(w, orchestrator.ErrUnavailable)
		return
	}
	stream, err := h.client.SubscribeEvents(requestContext(r), r.URL.Query().Get("after"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	for {
		event, err := stream.Recv()
		if err != nil {
			if errors.Is(err, r.Context().Err()) {
				return
			}
			return
		}
		payload, err := json.Marshal(map[string]string{
			"id": event.Id, "kind": event.Kind, "resourceId": event.ResourceId,
			"payload": event.PayloadJson, "createdAt": event.CreatedAt,
		})
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "id: %s\nevent: control-plane\ndata: %s\n\n", event.Id, payload); err != nil {
			return
		}
		flusher.Flush()
	}
}
