package handlers

import (
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type RunnerHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewRunnerHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *RunnerHandlers {
	return &RunnerHandlers{client: client, limiter: limiter}
}

func (h *RunnerHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/runners", auth.RequireSession(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/runners/{runner_id}/state", requireMutation(h.limiter, h.setState))
}

func (h *RunnerHandlers) setState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.SetRunnerState(requestContext(r), r.PathValue("runner_id"), body.State)
	if err != nil {
		writeClientError(w, err)
		return
	}
	runner := resp.Runner
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"id": runner.Id, "name": runner.Name, "enabled": runner.Enabled, "draining": runner.Draining, "status": runner.Status, "labels": runner.Labels, "lastSeenAt": runner.LastSeenAt})
}

func (h *RunnerHandlers) list(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListRunners(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type runnerResponse struct {
		ID         string   `json:"id"`
		Name       string   `json:"name"`
		Enabled    bool     `json:"enabled"`
		Draining   bool     `json:"draining"`
		Status     string   `json:"status"`
		Labels     []string `json:"labels"`
		LastSeenAt string   `json:"lastSeenAt"`
	}
	runners := make([]runnerResponse, len(resp.Runners))
	for i, r := range resp.Runners {
		runners[i] = runnerResponse{
			ID:         r.Id,
			Name:       r.Name,
			Enabled:    r.Enabled,
			Draining:   r.Draining,
			Status:     r.Status,
			Labels:     r.Labels,
			LastSeenAt: r.LastSeenAt,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"runners": runners})
}
