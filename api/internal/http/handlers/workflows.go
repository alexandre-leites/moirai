package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type WorkflowHandlers struct {
	client *orchestrator.Client
}

func NewWorkflowHandlers(client *orchestrator.Client) *WorkflowHandlers {
	return &WorkflowHandlers{client: client}
}

func (h *WorkflowHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/workflows", auth.RequireSession(http.HandlerFunc(h.list)))
}

func (h *WorkflowHandlers) list(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListWorkflows(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type workflowResponse struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
		Status    string `json:"status"`
		Phase     string `json:"phase"`
	}
	workflows := make([]workflowResponse, len(resp.Workflows))
	for i, wf := range resp.Workflows {
		workflows[i] = workflowResponse{
			ID:        wf.Id,
			ProjectID: wf.ProjectId,
			Status:    wf.Status,
			Phase:     wf.Phase,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"workflows": workflows})
}
