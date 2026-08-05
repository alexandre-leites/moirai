package handlers

import (
	"context"
	"net/http"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
)

// runnerClient is the slice of the orchestrator client these handlers use.
// Depending on the interface rather than the concrete *orchestrator.Client is
// what lets a test drive the handlers directly (same reason as eventClient).
type runnerClient interface {
	ListRunners(ctx context.Context) (*controlv1.ListRunnersResponse, error)
	SetRunnerState(ctx context.Context, runnerID, state string) (*controlv1.SetRunnerStateResponse, error)
}

type RunnerHandlers struct {
	client  runnerClient
	limiter *auth.RateLimiter
}

func NewRunnerHandlers(client runnerClient, limiter *auth.RateLimiter) *RunnerHandlers {
	return &RunnerHandlers{client: client, limiter: limiter}
}

func (h *RunnerHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/runners", auth.RequireSession(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/runners/{runner_id}/drain", requireMutation(h.limiter, h.drain))
	mux.Handle("POST /api/v1/runners/{runner_id}/undrain", requireMutation(h.limiter, h.undrain))
	mux.Handle("POST /api/v1/runners/{runner_id}/revoke", requireMutation(h.limiter, h.revoke))
}

func (h *RunnerHandlers) drain(w http.ResponseWriter, r *http.Request) {
	h.setState(w, r, "drain")
}

func (h *RunnerHandlers) undrain(w http.ResponseWriter, r *http.Request) {
	h.setState(w, r, "enable")
}

func (h *RunnerHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	h.setState(w, r, "revoke")
}

func (h *RunnerHandlers) setState(w http.ResponseWriter, r *http.Request, state string) {
	resp, err := h.client.SetRunnerState(requestContext(r), r.PathValue("runner_id"), state)
	if err != nil {
		writeClientError(w, err)
		return
	}
	if resp.Runner == nil {
		apiserver.WriteError(w, http.StatusServiceUnavailable, "Service unavailable", "runner state response is invalid")
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, runnerPayload(resp.Runner))
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
		Version    string   `json:"version"`
		Labels     []string `json:"labels"`
		LastSeenAt string   `json:"lastSeenAt"`
	}
	runners := make([]runnerResponse, len(resp.Runners))
	for i, r := range resp.Runners {
		labels := r.Labels
		if labels == nil {
			labels = []string{}
		}
		runners[i] = runnerResponse{
			ID:         r.Id,
			Name:       r.Name,
			Enabled:    r.Enabled,
			Draining:   r.Draining,
			Status:     r.Status,
			Version:    r.Version,
			Labels:     labels,
			LastSeenAt: r.LastSeenAt,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

func runnerPayload(runner *controlv1.Runner) map[string]any {
	labels := runner.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id": runner.Id, "name": runner.Name, "enabled": runner.Enabled,
		"draining": runner.Draining, "status": runner.Status, "version": runner.Version,
		"labels": labels, "lastSeenAt": runner.LastSeenAt,
	}
}
