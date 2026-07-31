package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

// Queue limits, mirrored by the orchestrator's list_queue (default 50,
// maximum 100). The API enforces them before calling the orchestrator so a
// bounded request never leaves this service.
const (
	queueDefaultLimit = 50
	queueMaxLimit     = 100
)

type QueueHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewQueueHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *QueueHandlers {
	return &QueueHandlers{client: client, limiter: limiter}
}

func (h *QueueHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/queue", auth.RequireSession(http.HandlerFunc(h.list)))
}

func (h *QueueHandlers) list(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt64(r, "limit", queueDefaultLimit, 1)
	if err != nil || limit > queueMaxLimit {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request", "limit must be between 1 and 100")
		return
	}
	resp, err := h.client.ListQueue(requestContext(r), int32(limit))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type queueEntry struct {
		ProjectID     string `json:"projectId"`
		ProjectName   string `json:"projectName"`
		ExternalID    string `json:"externalId"`
		Title         string `json:"title"`
		Priority      int32  `json:"priority"`
		BlockedReason string `json:"blockedReason"`
	}
	entries := make([]queueEntry, len(resp.Entries))
	for i, entry := range resp.Entries {
		entries[i] = queueEntry{
			ProjectID:     entry.ProjectId,
			ProjectName:   entry.ProjectName,
			ExternalID:    entry.ExternalId,
			Title:         entry.Title,
			Priority:      entry.Priority,
			BlockedReason: entry.BlockedReason,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
