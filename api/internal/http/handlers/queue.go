package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type QueueHandlers struct {
	client *orchestrator.Client
}

func NewQueueHandlers(client *orchestrator.Client) *QueueHandlers {
	return &QueueHandlers{client: client}
}

func (h *QueueHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/queue", auth.RequireSession(http.HandlerFunc(h.list)))
}

func (h *QueueHandlers) list(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListQueue(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	items := make([]map[string]any, len(resp.Items))
	for i, item := range resp.Items {
		items[i] = map[string]any{"workflowId": item.WorkflowRunId, "projectId": item.ProjectId, "issueId": item.IssueId, "priority": item.Priority, "status": item.Status, "phase": item.Phase, "queuedAt": item.QueuedAt}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
