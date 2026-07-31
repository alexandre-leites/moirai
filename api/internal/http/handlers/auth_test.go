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

func TestLogoutClearsCookies(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "test-session"})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.CSRFHeaderName, "test-csrf")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusNoContent)
	}
	cookies := rec.Result().Cookies()
	cleared := make(map[string]bool)
	for _, c := range cookies {
		if c.MaxAge == -1 && c.Value == "" {
			cleared[c.Name] = true
		}
	}
	if !cleared[auth.SessionCookieName] {
		t.Error("session cookie was not cleared")
	}
	if !cleared[auth.CSRFCookieName] {
		t.Error("CSRF cookie was not cleared")
	}
}

func TestLogoutIdempotentWithoutSession(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusNoContent)
	}
}
