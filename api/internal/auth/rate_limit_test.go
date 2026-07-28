package auth

import (
	"net"
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

func TestRateLimiterMiddlewareIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	limiter := NewRateLimiter(time.Minute, 1)
	h := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	// Two distinct "clients" both claim to be behind the same nginx peer, but that peer
	// is not in the trusted list, so each is keyed by its own untrusted RemoteAddr and
	// gets its own budget.
	for _, remote := range []string{"198.51.100.10:1", "198.51.100.11:1"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "203.0.113.99")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("remote %s: got %d, want %d", remote, rec.Code, http.StatusNoContent)
		}
	}
}

func TestRateLimiterMiddlewareHonorsForwardedForFromTrustedProxy(t *testing.T) {
	_, trusted, err := net.ParseCIDR("172.20.0.0/16")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	limiter := NewRateLimiter(time.Minute, 1, WithTrustedProxies([]*net.IPNet{trusted}))
	h := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	// Same client IP forwarded through the trusted nginx peer twice: second request
	// must be limited, because both share the forwarded client's budget.
	first := httptest.NewRequest(http.MethodPost, "/", nil)
	first.RemoteAddr = "172.20.0.5:1"
	first.Header.Set("X-Forwarded-For", "203.0.113.7, 172.20.0.5")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, first)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first request: got %d, want %d", rec.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodPost, "/", nil)
	second.RemoteAddr = "172.20.0.5:1"
	second.Header.Set("X-Forwarded-For", "203.0.113.7")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	// A different forwarded client through the same trusted proxy gets its own budget.
	other := httptest.NewRequest(http.MethodPost, "/", nil)
	other.RemoteAddr = "172.20.0.5:1"
	other.Header.Set("X-Forwarded-For", "203.0.113.8")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, other)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("other forwarded client: got %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRateLimiterEvictsExpiredEntries(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := NewRateLimiter(time.Minute, 1)
	limiter.now = func() time.Time { return now }
	for i := 0; i < 500; i++ {
		limiter.Allow(string(rune('a' + i%26)))
	}
	now = now.Add(2 * time.Minute)
	limiter.Allow("trigger-eviction")
	limiter.mu.Lock()
	remaining := len(limiter.entries)
	limiter.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("expected only the fresh entry to remain, got %d entries", remaining)
	}
}
