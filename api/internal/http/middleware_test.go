package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareSetsOrPreservesRequestIDAndSecurityHeaders(t *testing.T) {
	s, err := New(DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, requestedID := range []string{"", "caller-request-id"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if requestedID != "" {
			req.Header.Set(requestIDHeader, requestedID)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("got %d, want %d", rec.Code, http.StatusNoContent)
		}
		gotID := rec.Header().Get(requestIDHeader)
		if gotID == "" || (requestedID != "" && gotID != requestedID) {
			t.Errorf("request ID got %q for %q", gotID, requestedID)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Error("missing security headers")
		}
	}
}

func TestMiddlewareRecoversPanicsWithoutLeakingDetails(t *testing.T) {
	s, err := New(DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("secret panic") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "secret panic") {
		t.Fatal("panic details leaked")
	}
}
