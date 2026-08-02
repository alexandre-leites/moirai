package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loop-engineering/orchestrator/internal/metrics"
)

// loopSample reads one series out of the exposition text the metrics handler
// serves.
func loopSample(t *testing.T, server *metrics.Server, series string) (string, bool) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", recorder.Code)
	}
	for line := range strings.Lines(recorder.Body.String()) {
		name, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if found && name == series {
			return value, true
		}
	}
	return "", false
}

func TestObservedCountsEachPassByOutcome(t *testing.T) {
	server := metrics.New("", nil)
	outcome := error(nil)
	loop := observed(server.Recorder(), metrics.LoopIssueSync, func(context.Context) error { return outcome })

	if err := loop(context.Background()); err != nil {
		t.Fatalf("observed loop returned %v for a successful pass", err)
	}
	outcome = errors.New("gh: rate limited")
	if err := loop(context.Background()); err == nil {
		t.Fatal("observed swallowed the loop's error instead of passing it through")
	}

	if got, _ := loopSample(t, server, `moirai_orchestrator_loop_runs_total{loop="issue_sync",result="success"}`); got != "1" {
		t.Errorf("issue sync successes = %q, want 1", got)
	}
	if got, _ := loopSample(t, server, `moirai_orchestrator_loop_runs_total{loop="issue_sync",result="failure"}`); got != "1" {
		t.Errorf("issue sync failures = %q, want 1", got)
	}
}

// Shutdown cancels the context every loop runs under, so the last pass of a
// clean restart fails. Counting that would make every restart look like a
// reconciliation failure.
func TestObservedDoesNotCountAPassCutShortByShutdown(t *testing.T) {
	server := metrics.New("", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := observed(server.Recorder(), metrics.LoopRecoverySweep, func(ctx context.Context) error { return ctx.Err() })

	if err := loop(ctx); err == nil {
		t.Fatal("observed swallowed the cancellation")
	}

	if got, _ := loopSample(t, server, `moirai_orchestrator_loop_runs_total{loop="recovery_sweep",result="failure"}`); got != "0" {
		t.Errorf("recovery sweep failures = %q after a cancelled pass, want 0", got)
	}
}
