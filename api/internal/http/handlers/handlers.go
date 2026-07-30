package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

type ProjectHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewProjectHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *ProjectHandlers {
	return &ProjectHandlers{client: client, limiter: limiter}
}

func (h *ProjectHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/projects", auth.RequireSession(http.HandlerFunc(h.listProjects)))
	mux.Handle("POST /api/v1/projects", requireMutation(h.limiter, h.createProject))
	mux.Handle("PUT /api/v1/projects/{project_id}", requireMutation(h.limiter, h.updateProject))
	mux.Handle("POST /api/v1/projects/{project_id}/enable", requireMutation(h.limiter, h.enableProject))
	mux.Handle("POST /api/v1/projects/{project_id}/disable", requireMutation(h.limiter, h.disableProject))
}

func (h *ProjectHandlers) listProjects(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListProjects(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type projectResponse struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	projects := make([]projectResponse, len(resp.Projects))
	for i, p := range resp.Projects {
		projects[i] = projectResponse{ID: p.Id, Name: p.Name, Enabled: p.Enabled}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (h *ProjectHandlers) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name                 string   `json:"name"`
		RepositoryMode       string   `json:"repositoryMode"`
		RepositoryURL        string   `json:"repositoryUrl"`
		LocalRepositoryPath  string   `json:"localRepositoryPath"`
		DefaultBranch        string   `json:"defaultBranch"`
		RequiredRunnerLabels []string `json:"requiredRunnerLabels"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.CreateProject(requestContext(r), &controlv1.CreateProjectRequest{
		Project: &controlv1.ProjectConfiguration{
			Name:                 body.Name,
			RepositoryMode:       body.RepositoryMode,
			RepositoryUrl:        body.RepositoryURL,
			LocalRepositoryPath:  body.LocalRepositoryPath,
			DefaultBranch:        body.DefaultBranch,
			RequiredRunnerLabels: body.RequiredRunnerLabels,
		},
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusCreated, projectPayload(resp.Project))
}

func (h *ProjectHandlers) updateProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	var body struct {
		Name                 string   `json:"name"`
		RepositoryMode       string   `json:"repositoryMode"`
		RepositoryURL        string   `json:"repositoryUrl"`
		LocalRepositoryPath  string   `json:"localRepositoryPath"`
		DefaultBranch        string   `json:"defaultBranch"`
		RequiredRunnerLabels []string `json:"requiredRunnerLabels"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.UpdateProject(requestContext(r), &controlv1.UpdateProjectRequest{
		ProjectId: projectID,
		Project: &controlv1.ProjectConfiguration{
			Name:                 body.Name,
			RepositoryMode:       body.RepositoryMode,
			RepositoryUrl:        body.RepositoryURL,
			LocalRepositoryPath:  body.LocalRepositoryPath,
			DefaultBranch:        body.DefaultBranch,
			RequiredRunnerLabels: body.RequiredRunnerLabels,
		},
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, projectPayload(resp.Project))
}

func (h *ProjectHandlers) enableProject(w http.ResponseWriter, r *http.Request) {
	h.setProjectEnabled(w, r, true)
}

func (h *ProjectHandlers) disableProject(w http.ResponseWriter, r *http.Request) {
	h.setProjectEnabled(w, r, false)
}

func (h *ProjectHandlers) setProjectEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	projectID := r.PathValue("project_id")
	resp, err := h.client.SetProjectEnabled(requestContext(r), projectID, enabled)
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, projectPayload(resp.Project))
}

func projectPayload(p *controlv1.Project) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{"id": p.Id, "name": p.Name, "enabled": p.Enabled}
}

type RunnerTokenHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewRunnerTokenHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *RunnerTokenHandlers {
	return &RunnerTokenHandlers{client: client, limiter: limiter}
}

func (h *RunnerTokenHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/runner-tokens", auth.RequireSession(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/runner-tokens", requireMutation(h.limiter, h.create))
	mux.Handle("DELETE /api/v1/runner-tokens/{token_id}", requireMutation(h.limiter, h.revoke))
}

func (h *RunnerTokenHandlers) list(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListRunnerRegistrationTokens(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type tokenResponse struct {
		ID            string   `json:"id"`
		AllowedLabels []string `json:"allowedLabels"`
		ExpiresAt     string   `json:"expiresAt"`
		UsedAt        string   `json:"usedAt,omitempty"`
		RevokedAt     string   `json:"revokedAt,omitempty"`
	}
	tokens := make([]tokenResponse, len(resp.Tokens))
	for i, t := range resp.Tokens {
		tokens[i] = tokenResponse{
			ID:            t.Id,
			AllowedLabels: t.AllowedLabels,
			ExpiresAt:     t.ExpiresAt,
			UsedAt:        t.UsedAt,
			RevokedAt:     t.RevokedAt,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (h *RunnerTokenHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AllowedLabels []string `json:"allowedLabels"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.CreateRunnerRegistrationToken(requestContext(r), body.AllowedLabels)
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusCreated, map[string]any{
		"token":     resp.Token,
		"expiresAt": resp.ExpiresAt,
	})
}

func (h *RunnerTokenHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	tokenID := r.PathValue("token_id")
	_, err := h.client.RevokeRunnerRegistrationToken(requestContext(r), tokenID)
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func requireMutation(limiter *auth.RateLimiter, handler http.HandlerFunc) http.Handler {
	return auth.RequireSession(auth.RequireCSRF(limiter.SessionMiddleware(handler)))
}

func requestContext(r *http.Request) context.Context {
	if token, ok := auth.SessionToken(r.Context()); ok {
		csrfToken, _ := auth.CSRFToken(r.Context())
		return orchestrator.WithSession(r.Context(), token, csrfToken)
	}
	return r.Context()
}

func decodeJSON(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeClientError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrUnauthorized):
		apiserver.WriteError(w, http.StatusUnauthorized, "Unauthorized", "")
	case errors.Is(err, orchestrator.ErrForbidden):
		apiserver.WriteError(w, http.StatusForbidden, "Forbidden", "")
	case errors.Is(err, orchestrator.ErrInvalidInput):
		apiserver.WriteError(w, http.StatusUnprocessableEntity, "Validation error", err.Error())
	case errors.Is(err, orchestrator.ErrNotFound):
		apiserver.WriteError(w, http.StatusNotFound, "Not found", "")
	case errors.Is(err, context.DeadlineExceeded):
		apiserver.WriteError(w, http.StatusGatewayTimeout, "Orchestrator timed out", "")
	default:
		apiserver.WriteError(w, http.StatusServiceUnavailable, "Service unavailable", "")
	}
}
