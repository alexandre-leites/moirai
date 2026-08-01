package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

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
	mux.Handle("GET /api/v1/workflows/{workflow_id}/events", auth.RequireSession(http.HandlerFunc(h.listEvents)))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/decision", requireMutation(h.limiter, h.submitDecision))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/retry", requireMutation(h.limiter, h.retry))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/cancel", requireMutation(h.limiter, h.cancel))
	mux.Handle("POST /api/v1/workflows/{workflow_id}/block", requireMutation(h.limiter, h.block))
}

// list serves the same per-workflow shape as `get`. The console's workflow
// list, overview triage and phase threads all read issue titles, pull requests
// and attempt counters, and re-fetching each row individually would be a
// request per workflow; the orchestrator already returns these fields in one
// query.
func (h *WorkflowHandlers) list(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListWorkflows(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	workflows := make([]map[string]any, len(resp.Workflows))
	for i, workflow := range resp.Workflows {
		workflows[i] = workflowDetailPayload(workflow)
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"workflows": workflows})
}

func (h *WorkflowHandlers) get(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.GetWorkflow(requestContext(r), r.PathValue("workflow_id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	if resp.Workflow == nil {
		apiserver.WriteError(w, http.StatusServiceUnavailable, "Service unavailable", "workflow response is missing")
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, workflowDetailPayload(resp.Workflow))
}

func (h *WorkflowHandlers) listEvents(w http.ResponseWriter, r *http.Request) {
	cursor, err := queryInt64(r, "cursor", 0, 0)
	if err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request", "cursor must be a non-negative integer")
		return
	}
	limit, err := queryInt64(r, "limit", 100, 1)
	if err != nil || limit > 100 {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request", "limit must be between 1 and 100")
		return
	}
	resp, err := h.client.ListWorkflowEvents(requestContext(r), r.PathValue("workflow_id"), cursor, int32(limit))
	if err != nil {
		writeClientError(w, err)
		return
	}
	events := make([]map[string]any, len(resp.Events))
	for i, event := range resp.Events {
		payload := json.RawMessage(event.PayloadJson)
		if !json.Valid(payload) {
			payload = json.RawMessage("null")
		}
		events[i] = map[string]any{
			"id": event.Id, "type": event.EventType, "createdAt": event.CreatedAt, "payload": payload,
		}
	}
	body := map[string]any{"events": events}
	if resp.NextCursor != "" {
		body["nextCursor"] = resp.NextCursor
	}
	apiserver.WriteJSON(w, http.StatusOK, body)
}

func queryInt64(r *http.Request, name string, fallback, minimum int64) (int64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum {
		return 0, strconv.ErrSyntax
	}
	return parsed, nil
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

// workflowPayload is the shape the control RPCs answer with: those responses
// are built from the resumed graph state, not from a row read, so only the
// lifecycle fields are populated.
func workflowPayload(workflow *controlv1.Workflow) map[string]any {
	if workflow == nil {
		return nil
	}
	return map[string]any{
		"id": workflow.Id, "projectId": workflow.ProjectId,
		"status": workflow.Status, "phase": workflow.Phase,
	}
}

func workflowDetailPayload(workflow *controlv1.Workflow) map[string]any {
	payload := workflowPayload(workflow)
	if payload == nil {
		return nil
	}
	payload["issueExternalId"] = workflow.IssueExternalId
	payload["issueTitle"] = workflow.IssueTitle
	payload["branchName"] = workflow.BranchName
	payload["pullRequestExternalId"] = workflow.PullRequestExternalId
	payload["pullRequestUrl"] = workflow.PullRequestUrl
	payload["pullRequestState"] = workflow.PullRequestState
	payload["blockingReason"] = workflow.BlockingReason
	payload["planningAttempts"] = workflow.PlanningAttempts
	payload["implementationAttempts"] = workflow.ImplementationAttempts
	payload["pipelineRepairAttempts"] = workflow.PipelineRepairAttempts
	payload["ciRepairAttempts"] = workflow.CiRepairAttempts
	payload["reviewCycles"] = workflow.ReviewCycles
	payload["totalAgentExecutions"] = workflow.TotalAgentExecutions
	payload["createdAt"] = workflow.CreatedAt
	payload["updatedAt"] = workflow.UpdatedAt
	return payload
}
