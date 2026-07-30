package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
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
	mux.Handle("POST /api/v1/workflows/{workflow_id}/retry", requireMutation(h.limiter, h.retry))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/cancel", requireMutation(h.limiter, h.cancel))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/block", requireMutation(h.limiter, h.block))
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

func (h *WorkflowHandlers) retry(w http.ResponseWriter, r *http.Request) {
	h.control(w, r, "retry")
}

func (h *WorkflowHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	h.control(w, r, "cancel")
}

func (h *WorkflowHandlers) block(w http.ResponseWriter, r *http.Request) {
	h.control(w, r, "block")
}

func (h *WorkflowHandlers) control(w http.ResponseWriter, r *http.Request, action string) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if action == "block" && body.Reason == "" {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", "reason is required")
		return
	}
	workflowID := r.PathValue("workflow_id")
	var workflow *controlv1.Workflow
	var err error
	switch action {
	case "retry":
		resp, callErr := h.client.RetryWorkflow(requestContext(r), workflowID, body.Reason)
		err = callErr
		if resp != nil {
			workflow = resp.Workflow
		}
	case "cancel":
		resp, callErr := h.client.CancelWorkflow(requestContext(r), workflowID, body.Reason)
		err = callErr
		if resp != nil {
			workflow = resp.Workflow
		}
	case "block":
		resp, callErr := h.client.BlockWorkflow(requestContext(r), workflowID, body.Reason)
		err = callErr
		if resp != nil {
			workflow = resp.Workflow
		}
	}
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, workflowPayload(workflow))
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
	apiserver.WriteJSON(w, http.StatusOK, workflowPayload(resp.Workflow))
}

func workflowPayload(workflow *controlv1.Workflow) map[string]any {
	if workflow == nil {
		return nil
	}
	return map[string]any{
		"id": workflow.Id, "projectId": workflow.ProjectId,
		"status": workflow.Status, "phase": workflow.Phase,
	}
}
