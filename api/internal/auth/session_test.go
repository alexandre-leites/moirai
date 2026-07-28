package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequireSessionLoadsCookieToken(t *testing.T) {
	h := RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := SessionToken(r.Context())
		if !ok || token != "session-token" {
			t.Error("session token was not loaded")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "session-token"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("got %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRequireSessionAndCSRFFailClosed(t *testing.T) {
	session := RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	csr := RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, h := range []http.Handler{session, csr} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("unexpected status %d", rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf"})
	req.Header.Set(CSRFHeaderName, "different")
	rec := httptest.NewRecorder()
	csr.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRequireCSRFAcceptsMatchingToken(t *testing.T) {
	h := RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := CSRFToken(r.Context())
		if !ok || token != "csrf" {
			t.Error("CSRF token was not loaded")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: "csrf"})
	req.Header.Set(CSRFHeaderName, "csrf")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("got %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestSetAndClearSessionCookiesHaveSecureAttributes(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookies(rec, "session", "csrf", time.Now().Add(time.Hour), true)
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[1].HttpOnly || !cookies[1].Secure {
		t.Fatalf("unexpected set cookies: %#v", cookies)
	}
	rec = httptest.NewRecorder()
	ClearSessionCookies(rec, true)
	for _, line := range rec.Header().Values("Set-Cookie") {
		if !strings.Contains(line, "Max-Age=0") || !strings.Contains(line, "SameSite=Strict") {
			t.Errorf("unexpected clear cookie %q", line)
		}
	}
}
