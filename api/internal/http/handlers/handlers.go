package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

type pipelineStepPayload struct {
	Command        string `json:"command"`
	TimeoutSeconds int32  `json:"timeoutSeconds"`
	Position       int32  `json:"position"`
	Required       bool   `json:"required"`
}

func NewProjectHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *ProjectHandlers {
	return &ProjectHandlers{client: client, limiter: limiter}
}

func (h *ProjectHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/projects", auth.RequireSession(http.HandlerFunc(h.listProjects)))
	mux.Handle("POST /api/v1/projects", requireMutation(h.limiter, h.createProject))
	mux.Handle("PUT /api/v1/projects/{project_id}", requireMutation(h.limiter, h.updateProject))
	mux.Handle("GET /api/v1/projects/{project_id}/credentials", auth.RequireSession(http.HandlerFunc(h.listCredentials)))
	mux.Handle("PUT /api/v1/projects/{project_id}/credentials/{kind}", requireMutation(h.limiter, h.setCredential))
	mux.Handle("DELETE /api/v1/projects/{project_id}/credentials/{kind}", requireMutation(h.limiter, h.clearCredential))
	mux.Handle("POST /api/v1/projects/{project_id}/enable", requireMutation(h.limiter, h.enableProject))
	mux.Handle("POST /api/v1/projects/{project_id}/disable", requireMutation(h.limiter, h.disableProject))
	mux.Handle("GET /api/v1/scheduler/metrics", auth.RequireSession(http.HandlerFunc(h.schedulerMetrics)))
}

func (h *ProjectHandlers) schedulerMetrics(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.GetSchedulerMetrics(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type loopStatus struct {
		Name          string `json:"name"`
		Healthy       bool   `json:"healthy"`
		LastSuccessAt string `json:"lastSuccessAt,omitempty"`
		LastError     string `json:"lastError,omitempty"`
		LastErrorAt   string `json:"lastErrorAt,omitempty"`
	}
	loops := make([]loopStatus, len(resp.LoopStatuses))
	for i, status := range resp.LoopStatuses {
		loops[i] = loopStatus{
			Name:          status.Name,
			Healthy:       status.Healthy,
			LastSuccessAt: status.LastSuccessAt,
			LastError:     status.LastError,
			LastErrorAt:   status.LastErrorAt,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{
		"queueDepth":      resp.QueueDepth,
		"activeWorkflows": resp.ActiveWorkflows,
		"scheduledJobs":   resp.ScheduledJobs,
		"loops":           loops,
	})
}

func (h *ProjectHandlers) listProjects(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListProjects(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	projects := make([]map[string]any, len(resp.Projects))
	for i, p := range resp.Projects {
		projects[i] = projectPayload(p)
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (h *ProjectHandlers) createProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name                 string                `json:"name"`
		RepositoryMode       string                `json:"repositoryMode"`
		RepositoryURL        string                `json:"repositoryUrl"`
		LocalRepositoryPath  string                `json:"localRepositoryPath"`
		DefaultBranch        string                `json:"defaultBranch"`
		RequiredRunnerLabels []string              `json:"requiredRunnerLabels"`
		PipelineSteps        []pipelineStepPayload `json:"pipelineSteps"`
		ExecutionImage       string                `json:"executionImage"`
		RequireHumanApproval bool                  `json:"requireHumanApproval"`
		RequirePlanning      bool                  `json:"requirePlanning"`
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
			PipelineSteps:        pipelineSteps(body.PipelineSteps),
			ExecutionImage:       body.ExecutionImage,
			RequireHumanApproval: body.RequireHumanApproval,
			RequirePlanning:      body.RequirePlanning,
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
		Name                 string                `json:"name"`
		RepositoryMode       string                `json:"repositoryMode"`
		RepositoryURL        string                `json:"repositoryUrl"`
		LocalRepositoryPath  string                `json:"localRepositoryPath"`
		DefaultBranch        string                `json:"defaultBranch"`
		RequiredRunnerLabels []string              `json:"requiredRunnerLabels"`
		PipelineSteps        []pipelineStepPayload `json:"pipelineSteps"`
		ExecutionImage       string                `json:"executionImage"`
		RequireHumanApproval bool                  `json:"requireHumanApproval"`
		RequirePlanning      bool                  `json:"requirePlanning"`
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
			PipelineSteps:        pipelineSteps(body.PipelineSteps),
			ExecutionImage:       body.ExecutionImage,
			RequireHumanApproval: body.RequireHumanApproval,
			RequirePlanning:      body.RequirePlanning,
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

// Credential values travel inbound only. All three of these answer with the
// same summary of what a project has configured, so a caller never has to ask
// for a value back to know whether the write took effect -- and there is no
// endpoint that would return one if they did.
func (h *ProjectHandlers) listCredentials(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListProjectCredentials(requestContext(r), r.PathValue("project_id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, credentialsPayload(resp.GetCredentials()))
}

func (h *ProjectHandlers) setCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
		// Only meaningful for an "agent:<NAME>" kind: where the harness reads
		// this credential from, relative to the home directory the runner gives
		// the execution. Empty delivers it as an environment variable. A
		// destination, never a secret -- which is why, unlike the value, it is
		// also reported back.
		FilePath string `json:"filePath"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	if strings.TrimSpace(body.Value) == "" {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body",
			"value is required; delete the credential to remove it")
		return
	}
	resp, err := h.client.SetProjectCredential(
		requestContext(r), r.PathValue("project_id"), r.PathValue("kind"), body.Value, body.FilePath,
	)
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, credentialsPayload(resp.GetCredentials()))
}

func (h *ProjectHandlers) clearCredential(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ClearProjectCredential(
		requestContext(r), r.PathValue("project_id"), r.PathValue("kind"),
	)
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, credentialsPayload(resp.GetCredentials()))
}

func credentialsPayload(credentials []*controlv1.ProjectCredential) map[string]any {
	entries := make([]map[string]any, 0, len(credentials))
	for _, credential := range credentials {
		entries = append(entries, map[string]any{
			"kind":      credential.GetKind(),
			"createdAt": credential.GetCreatedAt(),
			"updatedAt": credential.GetUpdatedAt(),
			"filePath":  credential.GetFilePath(),
		})
	}
	return map[string]any{"credentials": entries}
}

func pipelineSteps(steps []pipelineStepPayload) []*controlv1.PipelineStep {
	result := make([]*controlv1.PipelineStep, len(steps))
	for i, step := range steps {
		result[i] = &controlv1.PipelineStep{
			Command: step.Command, TimeoutSeconds: step.TimeoutSeconds,
			Position: step.Position, Required: step.Required,
		}
	}
	return result
}

func projectPayload(p *controlv1.Project) map[string]any {
	if p == nil {
		return nil
	}
	labels := p.RequiredRunnerLabels
	if labels == nil {
		labels = []string{}
	}
	pipeline := make([]map[string]any, len(p.PipelineSteps))
	for i, step := range p.PipelineSteps {
		pipeline[i] = map[string]any{
			"command": step.Command, "timeoutSeconds": step.TimeoutSeconds,
			"position": step.Position, "required": step.Required,
		}
	}
	return map[string]any{
		"id":                   p.Id,
		"name":                 p.Name,
		"enabled":              p.Enabled,
		"repositoryMode":       p.RepositoryMode,
		"repositoryUrl":        redactURLUserinfo(p.RepositoryUrl),
		"localRepositoryPath":  p.LocalRepositoryPath,
		"defaultBranch":        p.DefaultBranch,
		"requiredRunnerLabels": labels,
		"pipelineSteps":        pipeline,
		"executionImage":       p.ExecutionImage,
		"requireHumanApproval": p.RequireHumanApproval,
		"requirePlanning":      p.RequirePlanning,
		"taskSources":          taskSourcesPayload(p.GetTaskSources()),
	}
}

// redactURLUserinfo strips userinfo (username[:password]) from http(s)
// repository URLs so credentials embedded in a stored clone URL never reach
// the browser. Other URL forms (scp-style, local paths) pass through untouched.
func redactURLUserinfo(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return raw
	}
	u.User = url.User("redacted")
	return u.String()
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
