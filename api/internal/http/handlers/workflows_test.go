package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
)

func TestWorkflowReadRoutesRequireSession(t *testing.T) {
	h := NewWorkflowHandlers(nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, path := range []string{"/api/v1/workflows/wf-1", "/api/v1/workflows/wf-1/events"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without session: got %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestWorkflowEventQueryRejectsInvalidCursorAndLimit(t *testing.T) {
	for _, target := range []string{"/api/v1/workflows/wf-1/events?cursor=-1", "/api/v1/workflows/wf-1/events?limit=101"} {
		h := NewWorkflowHandlers(nil, nil)
		rec := httptest.NewRecorder()
		h.listEvents(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestSubmitDecisionRejectsInvalidContentType(t *testing.T) {
	h := NewWorkflowHandlers(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/wf-1/decision", bytes.NewReader([]byte(`{"decision":"approved"}`)))
	req.SetPathValue("workflow_id", "wf-1")
	rec := httptest.NewRecorder()
	h.submitDecision(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestSubmitDecisionRejectsUnknownDecisionValue(t *testing.T) {
	h := NewWorkflowHandlers(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/wf-1/decision", bytes.NewReader([]byte(`{"decision":"maybe"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("workflow_id", "wf-1")
	rec := httptest.NewRecorder()
	h.submitDecision(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestBlockRejectsMissingReason(t *testing.T) {
	h := NewWorkflowHandlers(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/wf-1/block", bytes.NewReader([]byte(`{"reason":""}`)))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("workflow_id", "wf-1")
	rec := httptest.NewRecorder()
	h.block(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func workflowMux(client workflowClient) http.Handler {
	mux := http.NewServeMux()
	NewWorkflowHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

func sampleWorkflow(id string) *controlv1.Workflow {
	return &controlv1.Workflow{
		Id: id, ProjectId: "p-1", Status: "running", Phase: "implementation",
		IssueExternalId: "42", IssueTitle: "Fix the thing", BranchName: "issue-42",
		PullRequestExternalId: "77", PullRequestUrl: "https://example.test/pr/77",
		PullRequestState: "open", BlockingReason: "", PlanningAttempts: 1,
		ImplementationAttempts: 2, PipelineRepairAttempts: 3, CiRepairAttempts: 4,
		ReviewCycles: 5, TotalAgentExecutions: 6, PlanSummary: "do the thing",
		CreatedAt: "2026-08-01T00:00:00Z", UpdatedAt: "2026-08-02T00:00:00Z",
	}
}

// The console's list, overview triage and phase threads all read the detail
// fields straight off a list row, so `list` must serve the full shape, not the
// lifecycle-only one the control RPCs answer with.
func TestListWorkflowsServesTheFullDetailShape(t *testing.T) {
	stub := &stubClient{listWorkflows: func(context.Context) (*controlv1.ListWorkflowsResponse, error) {
		return &controlv1.ListWorkflowsResponse{Workflows: []*controlv1.Workflow{sampleWorkflow("wf-1")}}, nil
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/workflows", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Workflows []map[string]any `json:"workflows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Workflows) != 1 {
		t.Fatalf("workflows = %#v, want 1", body.Workflows)
	}
	row := body.Workflows[0]
	for key, want := range map[string]any{
		"id": "wf-1", "projectId": "p-1", "status": "running", "phase": "implementation",
		"issueExternalId": "42", "issueTitle": "Fix the thing", "branchName": "issue-42",
		"pullRequestUrl": "https://example.test/pr/77", "pullRequestState": "open",
		"planSummary": "do the thing", "createdAt": "2026-08-01T00:00:00Z",
	} {
		if row[key] != want {
			t.Errorf("%s = %#v, want %#v", key, row[key], want)
		}
	}
	if row["reviewCycles"] != float64(5) || row["totalAgentExecutions"] != float64(6) {
		t.Errorf("attempt counters = %#v / %#v", row["reviewCycles"], row["totalAgentExecutions"])
	}
}

func TestGetWorkflowForwardsThePathIDAndServesTheDetail(t *testing.T) {
	stub := &stubClient{getWorkflow: func(_ context.Context, id string) (*controlv1.GetWorkflowResponse, error) {
		return &controlv1.GetWorkflowResponse{Workflow: sampleWorkflow(id)}, nil
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/workflows/wf-9", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := stub.recorded("GetWorkflow")
	if len(calls) != 1 || calls[0].args[0] != "wf-9" {
		t.Fatalf("GetWorkflow calls = %#v, want one for wf-9", calls)
	}
	body := decodeProject(t, rec)
	if body["id"] != "wf-9" || body["issueTitle"] != "Fix the thing" {
		t.Fatalf("payload = %#v", body)
	}
}

func TestGetWorkflowSurfacesAnUnknownWorkflowAs404(t *testing.T) {
	stub := &stubClient{getWorkflow: func(context.Context, string) (*controlv1.GetWorkflowResponse, error) {
		return nil, orchestrator.ErrNotFound
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/workflows/nope", "", "admin-session"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A 200 with a null body would leave the console rendering an empty detail
// page as if the workflow existed.
func TestGetWorkflowRejectsAResponseWithNoWorkflow(t *testing.T) {
	stub := &stubClient{getWorkflow: func(context.Context, string) (*controlv1.GetWorkflowResponse, error) {
		return &controlv1.GetWorkflowResponse{}, nil
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/workflows/wf-1", "", "admin-session"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestListWorkflowEventsPagesAndPassesPayloadsThrough(t *testing.T) {
	stub := &stubClient{listWorkflowEvents: func(_ context.Context, id string, afterID int64, limit int32) (*controlv1.ListWorkflowEventsResponse, error) {
		return &controlv1.ListWorkflowEventsResponse{
			Events: []*controlv1.WorkflowEvent{
				{Id: "11", EventType: "phase_started", CreatedAt: "2026-08-01T00:00:00Z", PayloadJson: `{"phase":"planning"}`},
				// Not valid JSON: the handler must substitute null rather than
				// emit a body the console cannot parse at all.
				{Id: "12", EventType: "broken", CreatedAt: "2026-08-01T00:01:00Z", PayloadJson: `{oops`},
			},
			NextCursor: "12",
		}, nil
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(
		t, http.MethodGet, "/api/v1/workflows/wf-3/events?cursor=10&limit=25", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := stub.recorded("ListWorkflowEvents")
	if len(calls) != 1 {
		t.Fatalf("ListWorkflowEvents calls = %d, want 1", len(calls))
	}
	if calls[0].args[0] != "wf-3" || calls[0].args[1] != int64(10) || calls[0].args[2] != int32(25) {
		t.Fatalf("forwarded %#v, want [wf-3 10 25]", calls[0].args)
	}
	var body struct {
		Events []struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			CreatedAt string          `json:"createdAt"`
			Payload   json.RawMessage `json:"payload"`
		} `json:"events"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.NextCursor != "12" {
		t.Errorf("nextCursor = %q, want 12", body.NextCursor)
	}
	if len(body.Events) != 2 {
		t.Fatalf("events = %#v, want 2", body.Events)
	}
	if body.Events[0].ID != "11" || body.Events[0].Type != "phase_started" ||
		string(body.Events[0].Payload) != `{"phase":"planning"}` {
		t.Errorf("first event = %#v", body.Events[0])
	}
	if string(body.Events[1].Payload) != "null" {
		t.Errorf("invalid payload = %s, want null", body.Events[1].Payload)
	}
}

// The cursor is omitted from the body when the page is the last one, so the
// console stops paging instead of re-requesting from the same place forever.
func TestListWorkflowEventsOmitsTheCursorOnTheLastPage(t *testing.T) {
	stub := &stubClient{listWorkflowEvents: func(_ context.Context, _ string, afterID int64, limit int32) (*controlv1.ListWorkflowEventsResponse, error) {
		if afterID != 0 || limit != 100 {
			t.Errorf("defaults = cursor %d limit %d, want 0 and 100", afterID, limit)
		}
		return &controlv1.ListWorkflowEventsResponse{}, nil
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, projectRequest(
		t, http.MethodGet, "/api/v1/workflows/wf-3/events", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := decodeProject(t, rec)["nextCursor"]; ok {
		t.Errorf("last page still carried a cursor: %s", rec.Body.String())
	}
}

func TestListWorkflowEventsRejectsOutOfRangePaging(t *testing.T) {
	for _, query := range []string{"?cursor=-1", "?cursor=abc", "?limit=0", "?limit=101", "?limit=x"} {
		stub := &stubClient{}
		rec := httptest.NewRecorder()
		workflowMux(stub).ServeHTTP(rec, projectRequest(
			t, http.MethodGet, "/api/v1/workflows/wf-3/events"+query, "", "admin-session"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
		if len(stub.calls) != 0 {
			t.Errorf("%s: a rejected request still reached the orchestrator: %#v", query, stub.calls)
		}
	}
}

func TestWorkflowControlRoutesForwardTheIDAndReason(t *testing.T) {
	for _, tc := range []struct{ path, method string }{
		{"retry", "RetryWorkflow"},
		{"cancel", "CancelWorkflow"},
		{"block", "BlockWorkflow"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			answer := &controlv1.Workflow{Id: "wf-5", ProjectId: "p-1", Status: "queued", Phase: "planning"}
			stub := &stubClient{
				retryWorkflow: func(context.Context, string, string, bool) (*controlv1.RetryWorkflowResponse, error) {
					return &controlv1.RetryWorkflowResponse{Workflow: answer}, nil
				},
				cancelWorkflow: func(context.Context, string, string) (*controlv1.CancelWorkflowResponse, error) {
					return &controlv1.CancelWorkflowResponse{Workflow: answer}, nil
				},
				blockWorkflow: func(context.Context, string, string) (*controlv1.BlockWorkflowResponse, error) {
					return &controlv1.BlockWorkflowResponse{Workflow: answer}, nil
				},
			}
			rec := httptest.NewRecorder()
			workflowMux(stub).ServeHTTP(rec, mutateRequest(
				t, http.MethodPost, "/api/v1/workflows/wf-5/"+tc.path, `{"reason":"operator asked"}`, "admin-session"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			calls := stub.recorded(tc.method)
			if len(calls) != 1 || calls[0].args[0] != "wf-5" || calls[0].args[1] != "operator asked" {
				t.Fatalf("%s calls = %#v, want one for [wf-5 operator asked]", tc.method, calls)
			}
			body := decodeProject(t, rec)
			// The control RPCs answer from resumed graph state, so only the
			// lifecycle fields belong in the response.
			if body["id"] != "wf-5" || body["status"] != "queued" || body["phase"] != "planning" {
				t.Fatalf("payload = %#v", body)
			}
			if _, ok := body["issueTitle"]; ok {
				t.Errorf("control response carried detail fields: %#v", body)
			}
		})
	}
}

// A retry with `resume: true` is forwarded as a retry-with-context, not a
// fresh one: the RPC distinguishes the two on the resume flag.
func TestRetryWithContextForwardsResumeToTheOrchestrator(t *testing.T) {
	answer := &controlv1.Workflow{Id: "wf-5", ProjectId: "p-1", Status: "preparing", Phase: "preparing"}
	stub := &stubClient{
		retryWorkflow: func(_ context.Context, _ string, _ string, resume bool) (*controlv1.RetryWorkflowResponse, error) {
			if !resume {
				t.Fatal("resume flag was not forwarded")
			}
			return &controlv1.RetryWorkflowResponse{Workflow: answer}, nil
		},
	}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/workflows/wf-5/retry", `{"resume":true}`, "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowControlRejectsAMalformedBody(t *testing.T) {
	for _, path := range []string{"retry", "cancel", "block"} {
		stub := &stubClient{}
		rec := httptest.NewRecorder()
		workflowMux(stub).ServeHTTP(rec, mutateRequest(
			t, http.MethodPost, "/api/v1/workflows/wf-5/"+path, `{"unknown":"x"}`, "admin-session"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, rec.Code)
		}
		if len(stub.calls) != 0 {
			t.Errorf("%s: a rejected request still reached the orchestrator: %#v", path, stub.calls)
		}
	}
}

// Blocking a workflow without a reason leaves an operator with no record of
// why it stopped, so it is rejected before the RPC is made.
func TestBlockWithoutAReasonNeverReachesTheOrchestrator(t *testing.T) {
	stub := &stubClient{}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/workflows/wf-5/block", `{"reason":""}`, "admin-session"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("block without a reason reached the orchestrator: %#v", stub.calls)
	}
}

func TestWorkflowControlSurfacesOrchestratorRejection(t *testing.T) {
	stub := &stubClient{retryWorkflow: func(context.Context, string, string, bool) (*controlv1.RetryWorkflowResponse, error) {
		return nil, orchestrator.ErrInvalidInput
	}}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/workflows/wf-5/retry", `{"reason":"again"}`, "admin-session"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestSubmitDecisionForwardsTheDecisionAndComment(t *testing.T) {
	for _, decision := range []string{"approved", "changes_requested"} {
		t.Run(decision, func(t *testing.T) {
			stub := &stubClient{submitDecision: func(context.Context, string, string, string) (*controlv1.SubmitHumanDecisionResponse, error) {
				return &controlv1.SubmitHumanDecisionResponse{Workflow: &controlv1.Workflow{
					Id: "wf-2", ProjectId: "p-1", Status: "running", Phase: "review",
				}}, nil
			}}
			rec := httptest.NewRecorder()
			workflowMux(stub).ServeHTTP(rec, mutateRequest(t, http.MethodPost,
				"/api/v1/workflows/wf-2/decision",
				`{"decision":"`+decision+`","comment":"looks fine"}`, "admin-session"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			calls := stub.recorded("SubmitHumanDecision")
			if len(calls) != 1 {
				t.Fatalf("SubmitHumanDecision calls = %d, want 1", len(calls))
			}
			if calls[0].args[0] != "wf-2" || calls[0].args[1] != decision || calls[0].args[2] != "looks fine" {
				t.Fatalf("forwarded %#v", calls[0].args)
			}
			if body := decodeProject(t, rec); body["id"] != "wf-2" || body["phase"] != "review" {
				t.Fatalf("payload = %#v", body)
			}
		})
	}
}

func TestSubmitDecisionRejectsAnUnknownDecisionBeforeCalling(t *testing.T) {
	stub := &stubClient{}
	rec := httptest.NewRecorder()
	workflowMux(stub).ServeHTTP(rec, mutateRequest(t, http.MethodPost,
		"/api/v1/workflows/wf-2/decision", `{"decision":"maybe"}`, "admin-session"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an invalid decision reached the orchestrator: %#v", stub.calls)
	}
}

func TestWorkflowMutationRoutesRequireSessionAndCSRF(t *testing.T) {
	stub := &stubClient{}
	mux := workflowMux(stub)
	for _, path := range []string{
		"/api/v1/workflows/wf-1/retry",
		"/api/v1/workflows/wf-1/cancel",
		"/api/v1/workflows/wf-1/block",
		"/api/v1/workflows/wf-1/decision",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, projectRequest(t, http.MethodPost, path, `{"reason":"x"}`, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: status = %d, want 401", path, rec.Code)
		}

		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, projectRequest(t, http.MethodPost, path, `{"reason":"x"}`, "admin-session"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF: status = %d, want 403", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/workflows", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("list without a session: status = %d, want 401", rec.Code)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", stub.calls)
	}
}
