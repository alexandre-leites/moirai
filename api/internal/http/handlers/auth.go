package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type AuthHandlers struct {
	client       *orchestrator.Client
	cookieSecure bool
	loginLimiter *auth.RateLimiter
}

func NewAuthHandlers(client *orchestrator.Client, cookieSecure bool, loginLimiter *auth.RateLimiter) *AuthHandlers {
	return &AuthHandlers{
		client:       client,
		cookieSecure: cookieSecure,
		loginLimiter: loginLimiter,
	}
}

func (h *AuthHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/login", h.loginLimiter.Middleware(http.HandlerFunc(h.login)))
	mux.Handle("POST /api/v1/auth/logout", auth.RequireSession(auth.RequireCSRF(http.HandlerFunc(h.logout))))
	mux.Handle("GET /api/v1/auth/me", auth.RequireSession(http.HandlerFunc(h.me)))
}

func (h *AuthHandlers) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if body.Username == "" || body.Password == "" {
		apiserver.WriteError(w, http.StatusUnprocessableEntity, "Validation error", "username and password are required")
		return
	}
	resp, err := h.client.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		if errors.Is(err, orchestrator.ErrUnauthorized) {
			apiserver.WriteError(w, http.StatusUnauthorized, "Login failed", "")
			return
		}
		writeClientError(w, err)
		return
	}
	sessionExpiry := time.Now().UTC().Add(8 * time.Hour)
	auth.SetSessionCookies(w, resp.SessionToken, resp.CsrfToken, sessionExpiry, h.cookieSecure)
	apiserver.WriteJSON(w, http.StatusOK, map[string]string{"userId": resp.UserId})
}

func (h *AuthHandlers) logout(w http.ResponseWriter, r *http.Request) {
	token, ok := auth.SessionToken(r.Context())
	if ok && token != "" && h.client != nil {
		ctx := orchestrator.WithSession(r.Context(), token)
		_ = h.client.Logout(ctx)
	}
	auth.ClearSessionCookies(w, h.cookieSecure)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandlers) me(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.WhoAmI(requestContext(r))
	if err != nil {
		if errors.Is(err, orchestrator.ErrUnauthorized) {
			apiserver.WriteError(w, http.StatusUnauthorized, "Unauthorized", "")
			return
		}
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]string{
		"userId":   resp.UserId,
		"username": resp.Username,
		"role":     resp.Role,
	})
}
