package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type WorkflowHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewWorkflowHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *WorkflowHandlers {
	return &WorkflowHandlers{client: client, limiter: limiter}
}

func (h *WorkflowHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/workflows", auth.RequireSession(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/decision", requireMutation(h.limiter, h.submitDecision))
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

func (h *WorkflowHandlers) submitDecision(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("workflow_id")
	var body struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if body.Decision != "approved" && body.Decision != "changes_requested" {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", "decision must be approved or changes_requested")
		return
	}
	resp, err := h.client.SubmitHumanDecision(requestContext(r), workflowID, body.Decision, body.Comment)
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{
		"id":        resp.Workflow.Id,
		"projectId": resp.Workflow.ProjectId,
		"status":    resp.Workflow.Status,
		"phase":     resp.Workflow.Phase,
	})
}
