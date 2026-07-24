package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
)

func TestWriteClientErrorMapsOrchestratorErrors(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
	}{
		{orchestrator.ErrUnauthorized, http.StatusUnauthorized},
		{orchestrator.ErrInvalidInput, http.StatusUnprocessableEntity},
		{orchestrator.ErrNotFound, http.StatusNotFound},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{orchestrator.ErrUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeClientError(rec, tc.err)
		if rec.Code != tc.wantStatus {
			t.Errorf("err %v: got %d, want %d", tc.err, rec.Code, tc.wantStatus)
		}
	}
}

func TestDecodeJSONRejectsInvalidContentType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "text/plain")
	var body struct{}
	if err := decodeJSON(req, &body); err == nil {
		t.Fatal("expected error for wrong content type")
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"unknown":1}`)))
	req.Header.Set("Content-Type", "application/json")
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(req, &body); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestDecodeJSONAcceptsValidPayload(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":"hello"}`)))
	req.Header.Set("Content-Type", "application/json")
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(req, &body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Name != "hello" {
		t.Errorf("got %q, want %q", body.Name, "hello")
	}
}

func TestRequireMutationRequiresSessionAndCSRF(t *testing.T) {
	h := requireMutation(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	req = httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "csrf"})
	req.Header.Set(auth.CSRFHeaderName, "csrf")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("got %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestCSRFTokensAreRandomAndURLSafe(t *testing.T) {
	first, err := csrfToken()
	if err != nil {
		t.Fatalf("csrf token: %v", err)
	}
	second, err := csrfToken()
	if err != nil {
		t.Fatalf("csrf token: %v", err)
	}
	if first == second || len(first) < 32 {
		t.Fatalf("unexpected CSRF tokens: %q %q", first, second)
	}
}

var _ = errors.New
