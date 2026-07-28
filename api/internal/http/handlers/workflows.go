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
	mux.Handle("GET /api/v1/workflows/{workflow_id}", auth.RequireSession(http.HandlerFunc(h.get)))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/decision", requireMutation(h.limiter, h.submitDecision))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/retry", requireMutation(h.limiter, h.action("retry")))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/cancel", requireMutation(h.limiter, h.action("cancel")))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/block", requireMutation(h.limiter, h.action("block")))
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

func (h *WorkflowHandlers) get(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.GetWorkflow(requestContext(r), r.PathValue("workflow_id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	events := make([]map[string]string, len(resp.Events))
	for i, event := range resp.Events {
		events[i] = map[string]string{"id": event.Id, "type": event.EventType, "severity": event.Severity, "payload": event.PayloadJson, "createdAt": event.CreatedAt}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"workflow": workflowPayload(resp.Workflow), "events": events})
}

func (h *WorkflowHandlers) action(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reason string `json:"reason"`
		}
		if r.ContentLength != 0 {
			if err := decodeJSON(r, &body); err != nil {
				apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
				return
			}
		}
		resp, err := h.client.WorkflowAction(requestContext(r), r.PathValue("workflow_id"), action, body.Reason)
		if err != nil {
			writeClientError(w, err)
			return
		}
		apiserver.WriteJSON(w, http.StatusOK, workflowPayload(resp.Workflow))
	}
}

func workflowPayload(workflow *controlv1.Workflow) map[string]any {
	if workflow == nil {
		return nil
	}
	return map[string]any{"id": workflow.Id, "projectId": workflow.ProjectId, "status": workflow.Status, "phase": workflow.Phase, "blockingReason": workflow.BlockingReason, "planningAttempts": workflow.PlanningAttempts, "implementationAttempts": workflow.ImplementationAttempts, "pipelineRepairAttempts": workflow.PipelineRepairAttempts, "reviewCycles": workflow.ReviewCycles, "ciRepairAttempts": workflow.CiRepairAttempts, "totalAgentExecutions": workflow.TotalAgentExecutions, "pullRequestUrl": workflow.PullRequestUrl}
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
