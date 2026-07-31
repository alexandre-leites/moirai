package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueueRouteRequiresSession(t *testing.T) {
	h := NewQueueHandlers(nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/queue without session: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestQueueRejectsInvalidLimits(t *testing.T) {
	for _, target := range []string{"/api/v1/queue?limit=0", "/api/v1/queue?limit=101", "/api/v1/queue?limit=abc"} {
		h := NewQueueHandlers(nil, nil)
		rec := httptest.NewRecorder()
		h.list(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: got %d, want %d", target, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestQueueAcceptsDefaultLimit(t *testing.T) {
	h := NewQueueHandlers(nil, nil)
	rec := httptest.NewRecorder()
	defer func() {
		// A nil client panics once validation passes; that proves validation
		// accepted the default limit and proceeded to call the client.
		if recover() == nil {
			t.Fatal("expected validation to pass through to the client")
		}
	}()
	h.list(rec, httptest.NewRequest(http.MethodGet, "/api/v1/queue", nil))
}
