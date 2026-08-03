package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

type AuthHandlers struct {
	client          *orchestrator.Client
	cookieSecure    bool
	loginLimiter    *auth.RateLimiter
	mutationLimiter *auth.RateLimiter
}

func NewAuthHandlers(client *orchestrator.Client, cookieSecure bool, loginLimiter, mutationLimiter *auth.RateLimiter) *AuthHandlers {
	return &AuthHandlers{
		client:          client,
		cookieSecure:    cookieSecure,
		loginLimiter:    loginLimiter,
		mutationLimiter: mutationLimiter,
	}
}

func (h *AuthHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/login", h.loginLimiter.Middleware(http.HandlerFunc(h.login)))
	mux.Handle("POST /api/v1/auth/logout", http.HandlerFunc(h.logout))
	mux.Handle("GET /api/v1/auth/me", auth.RequireSession(http.HandlerFunc(h.me)))
	mux.Handle("PUT /api/v1/auth/account", requireMutation(h.mutationLimiter, h.updateAccount))
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
	apiserver.WriteJSON(w, http.StatusOK, userPayload(resp))
}

func (h *AuthHandlers) updateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
		NewEmail        string `json:"newEmail"`
		DisplayName     string `json:"displayName"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.UpdateAccount(requestContext(r), &controlv1.UpdateAccountRequest{
		CurrentPassword: body.CurrentPassword,
		NewPassword:     body.NewPassword,
		NewEmail:        body.NewEmail,
		DisplayName:     body.DisplayName,
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, userPayload(resp))
}

func userPayload(resp interface {
	GetUserId() string
	GetUsername() string
	GetRole() string
	GetEmail() string
	GetDisplayName() string
}) map[string]string {
	return map[string]string{
		"userId":      resp.GetUserId(),
		"username":    resp.GetUsername(),
		"role":        resp.GetRole(),
		"email":       resp.GetEmail(),
		"displayName": resp.GetDisplayName(),
	}
}
