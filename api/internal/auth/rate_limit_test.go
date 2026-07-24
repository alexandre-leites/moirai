package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterBoundsRequestsAndResetsWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(time.Minute, 2)
	limiter.now = func() time.Time { return now }
	if !limiter.Allow("client") || !limiter.Allow("client") || limiter.Allow("client") {
		t.Fatal("unexpected limit result")
	}
	now = now.Add(time.Minute)
	if !limiter.Allow("client") {
		t.Fatal("expected window reset")
	}
}

func TestRateLimiterSessionMiddlewareKeysRequestsByLoadedSession(t *testing.T) {
	limiter := NewRateLimiter(time.Minute, 1)
	h := limiter.SessionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for index, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req = req.WithContext(WithSessionToken(req.Context(), "session-token"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("request %d got %d, want %d", index, rec.Code, want)
		}
	}
}

func TestRateLimiterMiddlewareKeysRequestsByRemoteIP(t *testing.T) {
	limiter := NewRateLimiter(time.Minute, 1)
	h := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for index, want := range []int{http.StatusNoContent, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "203.0.113.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("request %d got %d, want %d", index, rec.Code, want)
		}
	}
}
