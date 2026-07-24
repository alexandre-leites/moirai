package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"
)

const (
	SessionCookieName = "loop_session"
	CSRFCookieName    = "loop_csrf"
	CSRFHeaderName    = "X-CSRF-Token"
)

type sessionKey struct{}

func WithSessionToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, sessionKey{}, token)
}

func SessionToken(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(sessionKey{}).(string)
	return token, ok && token != ""
}

func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithSessionToken(r.Context(), cookie.Value)))
	})
}

func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(CSRFCookieName)
		if err != nil || cookie.Value == "" || r.Header.Get(CSRFHeaderName) == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.Header.Get(CSRFHeaderName))) != 1 {
			writeForbidden(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func SetSessionCookies(w http.ResponseWriter, sessionToken, csrfToken string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    csrfToken,
		Path:     "/",
		Expires:  expiresAt,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func ClearSessionCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{SessionCookieName, CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: name == SessionCookieName,
			Secure:   secure,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func writeForbidden(w http.ResponseWriter) {
	http.Error(w, "forbidden", http.StatusForbidden)
}
