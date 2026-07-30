package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestSubmitDecisionAcceptsApprovedAndChangesRequested(t *testing.T) {
	for _, decision := range []string{"approved", "changes_requested"} {
		t.Run(decision, func(t *testing.T) {
			h := NewWorkflowHandlers(nil, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/wf-1/decision", bytes.NewReader([]byte(`{"decision":"`+decision+`"}`)))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("workflow_id", "wf-1")
			rec := httptest.NewRecorder()
			defer func() {
				// A nil client panics once validation passes; that proves
				// validation accepted the value and proceeded to call the client.
				if recover() == nil {
					t.Fatal("expected validation to pass through to the client")
				}
			}()
			h.submitDecision(rec, req)
		})
	}
}
