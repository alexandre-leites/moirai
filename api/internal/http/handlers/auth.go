package handlers

import (
	"crypto/rand"
	"encoding/base64"
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

func NewAuthHandlers(client *orchestrator.Client, cookieSecure bool) *AuthHandlers {
	return &AuthHandlers{
		client:       client,
		cookieSecure: cookieSecure,
		loginLimiter: auth.NewRateLimiter(time.Minute, 10),
	}
}

func (h *AuthHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/login", h.loginLimiter.Middleware(http.HandlerFunc(h.login)))
	mux.Handle("POST /api/v1/auth/logout", auth.RequireSession(auth.RequireCSRF(http.HandlerFunc(h.logout))))
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
	csrfToken, err := csrfToken()
	if err != nil {
		apiserver.WriteError(w, http.StatusServiceUnavailable, "Session creation failed", "")
		return
	}
	sessionExpiry := time.Now().UTC().Add(8 * time.Hour)
	auth.SetSessionCookies(w, resp.SessionToken, csrfToken, sessionExpiry, h.cookieSecure)
	apiserver.WriteJSON(w, http.StatusOK, map[string]string{"userId": resp.UserId})
}

func csrfToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (h *AuthHandlers) logout(w http.ResponseWriter, r *http.Request) {
	_ = r
	auth.ClearSessionCookies(w, h.cookieSecure)
	w.WriteHeader(http.StatusNoContent)
}
