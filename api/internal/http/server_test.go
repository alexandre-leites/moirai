package server_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiserver "github.com/loop-engineering/api/internal/http"
)

func TestServerValidatesConfig(t *testing.T) {
	_, err := apiserver.New(apiserver.Config{BindAddress: ""}, nil)
	if err == nil {
		t.Fatal("expected error for empty bind address")
	}
	_, err = apiserver.New(apiserver.Config{BindAddress: "not-valid"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid bind address")
	}
}

func TestServerHealthRoutes(t *testing.T) {
	s, err := apiserver.New(apiserver.DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for _, path := range []string{"/live", "/ready", "/api/v1/health"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
}

func TestServerReadyAndHealthReflectOrchestratorState(t *testing.T) {
	healthy := true
	cfg := apiserver.DefaultConfig()
	cfg.OrchestratorHealthy = func() bool { return healthy }
	s, err := apiserver.New(cfg, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for _, path := range []string{"/ready", "/api/v1/health"} {
		rec := httptest.NewRecorder()
		s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s while healthy: got %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
	healthy = false
	for _, path := range []string{"/ready", "/api/v1/health"} {
		rec := httptest.NewRecorder()
		s.Mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s while unhealthy: got %d, want %d", path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

func TestWriteErrorSetsCorrectStatusAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	apiserver.WriteError(rec, http.StatusUnauthorized, "Unauthorized", "session required")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("got content-type %q, want application/problem+json", ct)
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	if err := apiserver.DefaultConfig().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
}

// The API exports only the metrics it owns. Queue depth, active workflow
// counts and the fleet-wide runner heartbeat age are derived from the database
// the API cannot reach, and used to be registered here as gauges permanently
// stuck at zero; the orchestrator exports the real ones (issue #124).
func TestMetricsExposesOnlyApiOwnedSeries(t *testing.T) {
	s, err := apiserver.New(apiserver.DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// A request-scoped metric has no series until a request has been served, so
	// one is served before the scrape.
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/live", nil))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d", rec.Code)
	}
	for _, name := range []string{"moirai_api_requests_total", "moirai_api_request_duration_seconds"} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Errorf("metrics response missing %s", name)
		}
	}
	for _, name := range []string{"moirai_queue_depth", "moirai_active_workflow_count", "moirai_runner_heartbeat_age_seconds"} {
		if strings.Contains(rec.Body.String(), name) {
			t.Errorf("metrics response still exports orchestrator-owned %s", name)
		}
	}
}

func TestWriteJSONSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	apiserver.WriteJSON(rec, http.StatusOK, map[string]string{"ok": "true"})
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
}

var _ = errors.New
