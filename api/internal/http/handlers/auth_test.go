package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
)

func TestAuthMeRequiresSession(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
