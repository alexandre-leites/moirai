package handlers

import (
	"io"
	"net/http"

	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

// SyncHandlers exposes manual issue synchronization and per-project sync
// status to the web console, so an operator can see that a registered project
// is (or is not) being picked up without waiting for the next background pass.
type SyncHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewSyncHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *SyncHandlers {
	return &SyncHandlers{client: client, limiter: limiter}
}

func (h *SyncHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/sync/status", auth.RequireSession(http.HandlerFunc(h.status)))
	mux.Handle("POST /api/v1/sync", requireMutation(h.limiter, h.syncNow))
}

// syncNow triggers an issue-synchronization pass. The request body is optional;
// an absent or empty body syncs every enabled project, a JSON body with
// "projectId" syncs only that project.
func (h *SyncHandlers) syncNow(w http.ResponseWriter, r *http.Request) {
	projectID := ""
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil && len(body) > 0 {
		var payload struct {
			ProjectID string `json:"projectId"`
		}
		if err := decodeJSON(r, &payload); err != nil {
			apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
			return
		}
		projectID = payload.ProjectID
	}
	resp, err := h.client.SyncNow(requestContext(r), projectID)
	if err != nil {
		writeClientError(w, err)
		return
	}
	type syncResult struct {
		ProjectID     string `json:"projectId"`
		SyncedIssues  int32  `json:"syncedIssues"`
		Error         string `json:"error,omitempty"`
	}
	results := make([]syncResult, len(resp.Results))
	for i, result := range resp.Results {
		results[i] = syncResult{
			ProjectID:    result.ProjectId,
			SyncedIssues: result.SyncedIssues,
			Error:        result.Error,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (h *SyncHandlers) status(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.IssueSyncStatus(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	type statusEntry struct {
		ProjectID          string `json:"projectId"`
		ProjectName        string `json:"projectName"`
		Enabled            bool   `json:"enabled"`
		IssueCount         int32  `json:"issueCount"`
		EligibleCount      int32  `json:"eligibleCount"`
		LastSyncedAt       string `json:"lastSyncedAt,omitempty"`
		ConsecutiveFailures int32  `json:"consecutiveFailures"`
		NextRetryAt        string `json:"nextRetryAt,omitempty"`
		LastError          string `json:"lastError,omitempty"`
		BackingOff         bool   `json:"backingOff"`
	}
	entries := make([]statusEntry, len(resp.Entries))
	for i, entry := range resp.Entries {
		entries[i] = statusEntry{
			ProjectID:           entry.ProjectId,
			ProjectName:         entry.ProjectName,
			Enabled:             entry.Enabled,
			IssueCount:          entry.IssueCount,
			EligibleCount:       entry.EligibleCount,
			LastSyncedAt:        entry.LastSyncedAt,
			ConsecutiveFailures: entry.ConsecutiveFailures,
			NextRetryAt:         entry.NextRetryAt,
			LastError:           entry.LastError,
			BackingOff:          entry.BackingOff,
		}
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
