package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
