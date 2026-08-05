package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	"google.golang.org/grpc/metadata"
)

func TestAuthMeRequiresSession(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
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
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
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

// A logout call with no session cookie at all answers 401, the same as every
// other session-protected route (see TestAuthMeRequiresSession). Deliberately
// not 204: the route needs the session cookie to identify which server-side
// session to revoke, so RequireSession must run before the handler, and there
// is nothing sensitive in telling an unauthenticated caller it isn't signed
// in — every sibling endpoint already does exactly that.
func TestLogoutRequiresSession(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// A logout call with a session cookie but no CSRF token is rejected the same
// way an account update would be: logout is a mutation (it revokes a
// server-side session) and the console always sends the CSRF header for it
// (see web/src/api.ts logout()).
func TestLogoutRequiresCSRF(t *testing.T) {
	h := NewAuthHandlers(nil, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "test-session"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// End-to-end through the real mux (not the handler directly): a valid
// session + CSRF pair must reach the orchestrator's Logout RPC with the
// session token that RequireSession pulled off the cookie. This is the
// regression test for the bug where logout was registered without
// RequireSession, so auth.SessionToken(ctx) was always empty and
// client.Logout was never called even though the response looked like a
// success (204, cookies cleared).
func TestLogoutRouteRevokesTheSessionServerSide(t *testing.T) {
	var gotSessionMD []string
	stub := &stubClient{logout: func(ctx context.Context) error {
		if md, ok := metadata.FromOutgoingContext(ctx); ok {
			gotSessionMD = md.Get("x-loop-session")
		}
		return nil
	}}
	mux := authMux(stub)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "captured-token"})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "test-csrf"})
	req.Header.Set(auth.CSRFHeaderName, "test-csrf")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := stub.recorded("Logout")
	if len(calls) != 1 {
		t.Fatalf("Logout calls = %#v, want 1 call carrying the session token", stub.calls)
	}
	// This is the crux of the bug: before the fix, RequireSession never ran on
	// this route, so auth.SessionToken(ctx) was always empty and this
	// metadata (and the whole Logout call) never happened even though the
	// response still looked like a successful 204.
	if len(gotSessionMD) != 1 || gotSessionMD[0] != "captured-token" {
		t.Fatalf("session token forwarded to the orchestrator = %#v, want [captured-token]", gotSessionMD)
	}
}

func authMux(client authClient) http.Handler {
	mux := http.NewServeMux()
	NewAuthHandlers(client, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

func TestLoginSetsTheSessionCookiesAndReturnsTheUser(t *testing.T) {
	stub := &stubClient{login: func(context.Context, string, string) (*controlv1.LoginResponse, error) {
		return &controlv1.LoginResponse{
			SessionToken: "session-value", CsrfToken: "csrf-value", UserId: "u-1",
		}, nil
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(
		t, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"hunter2"}`, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := stub.recorded("Login")
	if len(calls) != 1 || calls[0].args[0] != "admin" || calls[0].args[1] != "hunter2" {
		t.Fatalf("Login calls = %#v", calls)
	}
	if body := decodeProject(t, rec); body["userId"] != "u-1" {
		t.Errorf("payload = %#v", body)
	}
	set := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		set[c.Name] = c
	}
	session, ok := set[auth.SessionCookieName]
	if !ok || session.Value != "session-value" {
		t.Fatalf("session cookie = %#v", set[auth.SessionCookieName])
	}
	// The session cookie is the credential: it must never be readable by
	// scripts, and cookieSecure was requested.
	if !session.HttpOnly || !session.Secure {
		t.Errorf("session cookie flags: httpOnly = %v, secure = %v", session.HttpOnly, session.Secure)
	}
	csrf, ok := set[auth.CSRFCookieName]
	if !ok || csrf.Value != "csrf-value" {
		t.Fatalf("csrf cookie = %#v", set[auth.CSRFCookieName])
	}
	// The CSRF cookie has to be readable by the console to be echoed back.
	if csrf.HttpOnly {
		t.Errorf("csrf cookie is httpOnly, the console cannot echo it")
	}
	// The response body must not leak the tokens themselves.
	if bodyText := rec.Body.String(); strings.Contains(bodyText, "session-value") || strings.Contains(bodyText, "csrf-value") {
		t.Errorf("login body leaked a token: %s", bodyText)
	}
}

func TestLoginRejectsMissingCredentialsBeforeCalling(t *testing.T) {
	for _, body := range []string{`{}`, `{"username":"admin"}`, `{"password":"hunter2"}`, `{"username":"","password":""}`} {
		stub := &stubClient{}
		rec := httptest.NewRecorder()
		authMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodPost, "/api/v1/auth/login", body, ""))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", body, rec.Code)
		}
		if len(stub.calls) != 0 {
			t.Errorf("%s: reached the orchestrator: %#v", body, stub.calls)
		}
	}
}

func TestLoginRejectsAMalformedBody(t *testing.T) {
	for _, body := range []string{`{`, `{"user":"admin"}`} {
		stub := &stubClient{}
		rec := httptest.NewRecorder()
		authMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodPost, "/api/v1/auth/login", body, ""))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// A rejected login answers 401 with no detail: the message must not tell a
// caller whether the username or the password was the wrong one.
func TestLoginAnswers401WithoutDetailOnBadCredentials(t *testing.T) {
	stub := &stubClient{login: func(context.Context, string, string) (*controlv1.LoginResponse, error) {
		return nil, orchestrator.ErrUnauthorized
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(
		t, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"wrong"}`, ""))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("a failed login set cookies: %#v", rec.Result().Cookies())
	}
	if strings.Contains(rec.Body.String(), "admin") {
		t.Errorf("401 body echoed the username: %s", rec.Body.String())
	}
}

func TestLoginSurfacesAnUnreachableOrchestrator(t *testing.T) {
	stub := &stubClient{login: func(context.Context, string, string) (*controlv1.LoginResponse, error) {
		return nil, errors.New("connection refused")
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(
		t, http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"hunter2"}`, ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestMeReturnsTheSignedInUser(t *testing.T) {
	stub := &stubClient{whoAmI: func(context.Context) (*controlv1.WhoAmIResponse, error) {
		return &controlv1.WhoAmIResponse{
			UserId: "u-1", Username: "admin", Role: "administrator",
			Email: "admin@example.test", DisplayName: "Admin",
		}, nil
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/auth/me", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeProject(t, rec)
	for key, want := range map[string]any{
		"userId": "u-1", "username": "admin", "role": "administrator",
		"email": "admin@example.test", "displayName": "Admin",
	} {
		if body[key] != want {
			t.Errorf("%s = %#v, want %#v", key, body[key], want)
		}
	}
}

// An expired session must answer 401 so the console redirects to login rather
// than showing a service-unavailable banner the user cannot act on.
func TestMeAnswers401WhenTheSessionIsRejected(t *testing.T) {
	stub := &stubClient{whoAmI: func(context.Context) (*controlv1.WhoAmIResponse, error) {
		return nil, orchestrator.ErrUnauthorized
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/auth/me", "", "stale-session"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestMeSurfacesOtherFailuresAs503(t *testing.T) {
	stub := &stubClient{whoAmI: func(context.Context) (*controlv1.WhoAmIResponse, error) {
		return nil, errors.New("boom")
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/auth/me", "", "admin-session"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestUpdateAccountForwardsEveryFieldAndReturnsTheUser(t *testing.T) {
	stub := &stubClient{updateAccount: func(_ context.Context, req *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
		return &controlv1.UpdateAccountResponse{
			UserId: "u-1", Username: "admin", Role: "administrator",
			Email: req.GetNewEmail(), DisplayName: req.GetDisplayName(),
		}, nil
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, mutateRequest(t, http.MethodPut, "/api/v1/auth/account",
		`{"currentPassword":"old","newPassword":"new","newEmail":"a@b.test","displayName":"Ada"}`, "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := stub.recorded("UpdateAccount")
	if len(calls) != 1 {
		t.Fatalf("UpdateAccount calls = %d, want 1", len(calls))
	}
	req, _ := calls[0].args[0].(*controlv1.UpdateAccountRequest)
	if req == nil || req.GetCurrentPassword() != "old" || req.GetNewPassword() != "new" ||
		req.GetNewEmail() != "a@b.test" || req.GetDisplayName() != "Ada" {
		t.Fatalf("forwarded request = %#v", req)
	}
	body := decodeProject(t, rec)
	if body["email"] != "a@b.test" || body["displayName"] != "Ada" {
		t.Fatalf("payload = %#v", body)
	}
	// The passwords travel inbound only.
	if text := rec.Body.String(); strings.Contains(text, "old") || strings.Contains(text, "new") {
		t.Errorf("account response echoed a password: %s", text)
	}
}

func TestUpdateAccountRejectsAMalformedBody(t *testing.T) {
	for _, body := range []string{`{`, `{"password":"x"}`} {
		stub := &stubClient{}
		rec := httptest.NewRecorder()
		authMux(stub).ServeHTTP(rec, mutateRequest(t, http.MethodPut, "/api/v1/auth/account", body, "admin-session"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
		if len(stub.calls) != 0 {
			t.Errorf("%s: reached the orchestrator: %#v", body, stub.calls)
		}
	}
}

// A wrong current password is a validation failure, not a session failure:
// answering 401 would sign the user out of a session that is still valid.
func TestUpdateAccountSurfacesAWrongPasswordAs422(t *testing.T) {
	stub := &stubClient{updateAccount: func(context.Context, *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
		return nil, orchestrator.ErrInvalidInput
	}}
	rec := httptest.NewRecorder()
	authMux(stub).ServeHTTP(rec, mutateRequest(t, http.MethodPut, "/api/v1/auth/account",
		`{"currentPassword":"wrong","newPassword":"new"}`, "admin-session"))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestUpdateAccountRequiresSessionAndCSRF(t *testing.T) {
	stub := &stubClient{}
	mux := authMux(stub)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, projectRequest(t, http.MethodPut, "/api/v1/auth/account", `{}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a session: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, projectRequest(t, http.MethodPut, "/api/v1/auth/account", `{}`, "admin-session"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("without CSRF: status = %d, want 403", rec.Code)
	}

	if len(stub.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", stub.calls)
	}
}

// logoutRequest builds a request whose context already carries the session
// token, the way auth.RequireSession would after running on the real route
// (see TestLogoutRouteRevokesTheSessionServerSide for the end-to-end version
// that exercises RequireSession itself).
func logoutRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	return req.WithContext(auth.WithSessionToken(req.Context(), "session-value"))
}

// Logout revokes the session server-side as well as clearing the cookies; a
// cookie-only logout would leave the token usable by anything that captured it.
func TestLogoutRevokesTheSessionServerSide(t *testing.T) {
	stub := &stubClient{logout: func(context.Context) error { return nil }}
	h := NewAuthHandlers(stub, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
	rec := httptest.NewRecorder()

	h.logout(rec, logoutRequest())

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(stub.recorded("Logout")) != 1 {
		t.Fatalf("Logout calls = %#v, want 1", stub.calls)
	}
}

// An orchestrator that cannot be reached must not strand the browser with a
// live session cookie: the cookies are cleared either way.
func TestLogoutClearsCookiesEvenWhenRevocationFails(t *testing.T) {
	stub := &stubClient{logout: func(context.Context) error { return errors.New("unreachable") }}
	h := NewAuthHandlers(stub, true, auth.NewRateLimiter(time.Minute, 10), auth.NewRateLimiter(time.Minute, 60))
	rec := httptest.NewRecorder()

	h.logout(rec, logoutRequest())

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		cleared := false
		for _, c := range rec.Result().Cookies() {
			if c.Name == name && c.MaxAge == -1 && c.Value == "" {
				cleared = true
			}
		}
		if !cleared {
			t.Errorf("%s was not cleared", name)
		}
	}
}
