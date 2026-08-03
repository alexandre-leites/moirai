package main

import (
	"context"
	"errors"
	"net"
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

// This is the crux of issue #278: the healthcheck subcommand is a separate
// process invocation (os.Args[1] == "healthcheck"), so it cannot read the
// running server's in-memory loop state directly. It has to go through
// /readyz instead, and its own exit code has to track what that endpoint
// says -- 0 while ready, nonzero once a loop is reported stale.
func TestHealthcheckExitCodeTracksReadyz(t *testing.T) {
	ready := true
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	t.Setenv("LOOP_METRICS_BIND", strings.TrimPrefix(upstream.URL, "http://"))

	if code := healthcheckExitCode(); code != 0 {
		t.Fatalf("healthcheckExitCode() = %d while /readyz reports ready, want 0", code)
	}

	ready = false
	if code := healthcheckExitCode(); code == 0 {
		t.Fatal("healthcheckExitCode() = 0 while /readyz reports 503, want nonzero")
	}
}

// LOOP_METRICS_BIND="" is an explicit choice to run without a readiness
// endpoint at all, not an oversight. The healthcheck degrades to a bare TCP
// dial on the gRPC port rather than failing outright, since that channel does
// not exist for it to ask.
func TestHealthcheckFallsBackToLivenessWhenMetricsDisabled(t *testing.T) {
	t.Setenv("LOOP_METRICS_BIND", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("LOOP_GRPC_BIND", listener.Addr().String())

	if code := healthcheckExitCode(); code != 0 {
		t.Fatalf("healthcheckExitCode() = %d with metrics disabled but the gRPC port open, want 0", code)
	}

	listener.Close()
	if code := healthcheckExitCode(); code == 0 {
		t.Fatal("healthcheckExitCode() = 0 with metrics disabled and the gRPC port closed, want nonzero")
	}
}

// TestDrainScheduleStopsOnFalse pins the #290 fix: draining must place every
// job a tick has ready rather than the fixed one-per-tick rate discarding
// ScheduleOnce's bool used to produce, but it must also stop the moment
// nothing is left to claim instead of always spinning to the per-tick bound.
func TestDrainScheduleStopsOnFalse(t *testing.T) {
	calls := 0
	remaining := 7
	scheduleOnce := func(context.Context) (bool, error) {
		calls++
		if remaining == 0 {
			return false, nil
		}
		remaining--
		return true, nil
	}

	if err := drainSchedule(context.Background(), scheduleOnce); err != nil {
		t.Fatalf("drainSchedule: %v", err)
	}
	if calls != 8 {
		t.Fatalf("scheduleOnce was called %d times draining 7 eligible placements, want 8 (7 successes + the false that ends the drain)", calls)
	}
}

// TestDrainScheduleBoundsAVeryLargeQueue is the adversarial case: a queue deep
// enough that scheduleOnce would report true forever must not let one tick
// run unbounded and starve every other loop sharing this process. This drives
// a scheduleOnce that always succeeds and confirms drainSchedule still
// returns after exactly maxScheduledPerTick calls.
func TestDrainScheduleBoundsAVeryLargeQueue(t *testing.T) {
	calls := 0
	scheduleOnce := func(context.Context) (bool, error) {
		calls++
		return true, nil
	}

	if err := drainSchedule(context.Background(), scheduleOnce); err != nil {
		t.Fatalf("drainSchedule: %v", err)
	}
	if calls != maxScheduledPerTick {
		t.Fatalf("scheduleOnce was called %d times against a queue that never runs out, want exactly the per-tick bound %d", calls, maxScheduledPerTick)
	}
}

// TestDrainScheduleStopsOnError confirms a failure mid-drain is surfaced
// immediately rather than being swallowed while the loop keeps spinning.
func TestDrainScheduleStopsOnError(t *testing.T) {
	wantErr := errors.New("database says no")
	calls := 0
	scheduleOnce := func(context.Context) (bool, error) {
		calls++
		if calls == 3 {
			return false, wantErr
		}
		return true, nil
	}

	err := drainSchedule(context.Background(), scheduleOnce)
	if !errors.Is(err, wantErr) {
		t.Fatalf("drainSchedule: got %v, want %v", err, wantErr)
	}
	if calls != 3 {
		t.Fatalf("scheduleOnce was called %d times, want exactly 3 (the loop must stop at the first error)", calls)
	}
}

// TestDrainScheduleStopsOnCancelledContext confirms a drain in progress does
// not keep hammering the database once shutdown has already been signalled.
func TestDrainScheduleStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	scheduleOnce := func(context.Context) (bool, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return true, nil
	}

	if err := drainSchedule(ctx, scheduleOnce); err == nil {
		t.Fatal("drainSchedule: want the context's cancellation error once shutdown is signalled mid-drain")
	}
	if calls != 2 {
		t.Fatalf("scheduleOnce was called %d times after cancellation, want exactly 2 (the loop must not call it again once ctx is done)", calls)
	}
}

// readinessAddress must derive the port from LOOP_METRICS_BIND like the
// server itself does, and correctly tell "unset" (use the default, metrics
// on) apart from "set to empty" (metrics deliberately off).
func TestReadinessAddressHonoursExplicitDisablement(t *testing.T) {
	t.Setenv("LOOP_METRICS_BIND", "0.0.0.0:9999")
	address, ok := readinessAddress()
	if !ok {
		t.Fatal("readinessAddress() reported metrics disabled with LOOP_METRICS_BIND set to a port")
	}
	if address != "127.0.0.1:9999" {
		t.Errorf("readinessAddress() = %q, want 127.0.0.1:9999", address)
	}

	t.Setenv("LOOP_METRICS_BIND", "")
	if _, ok := readinessAddress(); ok {
		t.Error("readinessAddress() reported metrics enabled with LOOP_METRICS_BIND explicitly empty")
	}
}
