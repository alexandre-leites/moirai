package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubSource returns a fixed reading, or fails the way a database would.
type stubSource struct {
	snapshot Snapshot
	err      error
	panic    bool
	calls    int
}

func (s *stubSource) MetricsSnapshot(context.Context) (Snapshot, error) {
	s.calls++
	if s.panic {
		panic("pool is closed")
	}
	if s.err != nil {
		return Snapshot{}, s.err
	}
	return s.snapshot, nil
}

// scrape exercises the exact handler the listener serves and returns the
// exposition text, which is what a Prometheus server would actually read.
func scrape(t *testing.T, server *Server) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

// sample returns the value of one exported series, and reports whether it was
// exported at all. Absence is a meaningful outcome here, not a test failure.
func sample(body, series string) (float64, bool) {
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

func mustSample(t *testing.T, body, series string) float64 {
	t.Helper()
	value, exported := sample(body, series)
	if !exported {
		t.Fatalf("%s was not exported; body:\n%s", series, body)
	}
	return value
}

// The whole point of the exporter: the numbers a scrape reads are the numbers
// the database holds, not a constant. Issue #124 removed three gauges that were
// set to zero once and never written again.
func TestScrapeExportsTheStateItRead(t *testing.T) {
	source := &stubSource{snapshot: Snapshot{
		QueueDepth:         7,
		ActiveWorkflows:    3,
		ScheduledJobs:      2,
		EnabledRunners:     4,
		OldestHeartbeatAge: 90 * time.Second,
		HeartbeatKnown:     true,
	}}
	server := New("", source)

	body := scrape(t, server)

	for series, want := range map[string]float64{
		"moirai_queue_depth":                  7,
		"moirai_active_workflows":             3,
		"moirai_scheduled_jobs":               2,
		"moirai_enabled_runners":              4,
		"moirai_runner_heartbeat_age_seconds": 90,
	} {
		if got := mustSample(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	// A second scrape must re-read rather than replay: an age is only true at
	// the instant it is read.
	source.snapshot.QueueDepth = 11
	source.snapshot.OldestHeartbeatAge = 150 * time.Second
	body = scrape(t, server)
	if got := mustSample(t, body, "moirai_queue_depth"); got != 11 {
		t.Errorf("second scrape moirai_queue_depth = %v, want 11 (the exporter served a cached value)", got)
	}
	if got := mustSample(t, body, "moirai_runner_heartbeat_age_seconds"); got != 150 {
		t.Errorf("second scrape moirai_runner_heartbeat_age_seconds = %v, want 150", got)
	}
	if source.calls != 2 {
		t.Errorf("source read %d times, want 2 (one per scrape)", source.calls)
	}
}

// A zero would claim the queue is empty and every runner just checked in. The
// series are omitted instead, and the omission is itself counted.
func TestFailedReadOmitsTheStateSeriesInsteadOfReportingZero(t *testing.T) {
	server := New("", &stubSource{err: errors.New("connection refused")})

	body := scrape(t, server)

	for _, series := range []string{
		"moirai_queue_depth",
		"moirai_active_workflows",
		"moirai_scheduled_jobs",
		"moirai_enabled_runners",
		"moirai_runner_heartbeat_age_seconds",
	} {
		if value, exported := sample(body, series); exported {
			t.Errorf("%s = %v after a failed read, want the series to be absent", series, value)
		}
	}
	if got := mustSample(t, body, "moirai_orchestrator_metrics_scrape_errors_total"); got != 1 {
		t.Errorf("moirai_orchestrator_metrics_scrape_errors_total = %v, want 1", got)
	}
	// The loop series come from process state, so they survive a database that
	// does not answer.
	if _, exported := sample(body, `moirai_orchestrator_loop_last_success_age_seconds{loop="issue_sync"}`); !exported {
		t.Error("the loop age series disappeared with the database read; body:\n" + body)
	}
}

// A scrape runs on the registry's own goroutine, which has no recovery of its
// own: an unhandled panic there would take the orchestrator down with it.
func TestPanickingReadIsContainedAndCounted(t *testing.T) {
	source := &stubSource{panic: true}
	server := New("", source)

	body := scrape(t, server)

	if value, exported := sample(body, "moirai_queue_depth"); exported {
		t.Errorf("moirai_queue_depth = %v after a panicking read, want the series to be absent", value)
	}
	if got := mustSample(t, body, "moirai_orchestrator_metrics_scrape_errors_total"); got != 1 {
		t.Errorf("moirai_orchestrator_metrics_scrape_errors_total = %v, want 1", got)
	}
	// Still serving, and still reading: one bad scrape is not a terminal state.
	source.panic = false
	source.snapshot = Snapshot{QueueDepth: 5}
	if got := mustSample(t, scrape(t, server), "moirai_queue_depth"); got != 5 {
		t.Errorf("moirai_queue_depth = %v on the scrape after a panic, want 5", got)
	}
}

// With no runner enabled there is no fleet-wide heartbeat age. Exporting zero
// would read as "every runner just checked in", which is the lie issue #124
// exists to remove; the runner count is what tells the two cases apart.
func TestHeartbeatAgeIsAbsentWhenNoRunnerIsEnabled(t *testing.T) {
	server := New("", &stubSource{snapshot: Snapshot{QueueDepth: 1, EnabledRunners: 0, HeartbeatKnown: false}})

	body := scrape(t, server)

	if value, exported := sample(body, "moirai_runner_heartbeat_age_seconds"); exported {
		t.Errorf("moirai_runner_heartbeat_age_seconds = %v with no enabled runner, want the series to be absent", value)
	}
	if got := mustSample(t, body, "moirai_enabled_runners"); got != 0 {
		t.Errorf("moirai_enabled_runners = %v, want 0", got)
	}
	if got := mustSample(t, body, "moirai_queue_depth"); got != 1 {
		t.Errorf("moirai_queue_depth = %v, want 1: one absent series must not suppress the rest", got)
	}
}

func TestLoopOutcomesAreCountedAndTheirAgeGrowsWithTheClock(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()

	// Nothing has succeeded yet: the age counts from process start, so a loop
	// that never runs is still visibly stale.
	now = now.Add(45 * time.Second)
	if got := mustSample(t, scrape(t, server), `moirai_orchestrator_loop_last_success_age_seconds{loop="recovery_sweep"}`); got != 45 {
		t.Errorf("age before the first success = %v, want 45", got)
	}

	recorder.RecordLoopRun(LoopRecoverySweep, nil)
	recorder.RecordLoopRun(LoopIssueSync, errors.New("gh: rate limited"))
	now = now.Add(30 * time.Second)

	body := scrape(t, server)
	if got := mustSample(t, body, `moirai_orchestrator_loop_last_success_age_seconds{loop="recovery_sweep"}`); got != 30 {
		t.Errorf("recovery sweep age = %v, want 30 (the success did not reset it)", got)
	}
	if got := mustSample(t, body, `moirai_orchestrator_loop_last_success_age_seconds{loop="issue_sync"}`); got != 75 {
		t.Errorf("issue sync age = %v, want 75 (a failure must not count as a success)", got)
	}
	if got := mustSample(t, body, `moirai_orchestrator_loop_runs_total{loop="recovery_sweep",result="success"}`); got != 1 {
		t.Errorf("recovery sweep successes = %v, want 1", got)
	}
	if got := mustSample(t, body, `moirai_orchestrator_loop_runs_total{loop="issue_sync",result="failure"}`); got != 1 {
		t.Errorf("issue sync failures = %v, want 1", got)
	}
	// Every child exists from the first scrape, so rate() has a baseline before
	// the first failure rather than after it.
	if got := mustSample(t, body, `moirai_orchestrator_loop_runs_total{loop="recovery_sweep",result="failure"}`); got != 0 {
		t.Errorf("recovery sweep failures = %v, want 0", got)
	}
}

// The label set is closed, so a caller passing an unrecognised loop name cannot
// grow the series count.
func TestUnknownLoopNameFoldsIntoOneSeries(t *testing.T) {
	server := New("", &stubSource{})
	server.Recorder().RecordLoopRun("a-loop-nobody-declared", nil)

	body := scrape(t, server)

	if got := mustSample(t, body, `moirai_orchestrator_loop_runs_total{loop="unknown",result="success"}`); got != 1 {
		t.Errorf("unknown loop successes = %v, want 1", got)
	}
	if strings.Contains(body, "a-loop-nobody-declared") {
		t.Error("an unrecognised loop name minted its own series")
	}
}

// A nil recorder is what a caller wired before the metrics server exists holds.
// It must not panic.
func TestNilRecorderIsInert(t *testing.T) {
	var recorder *Recorder
	recorder.RecordLoopRun(LoopIssueSync, nil)
	var server *Server
	if server.Enabled() || server.Recorder() != nil || server.Addr() != "" {
		t.Error("a nil server reported itself as running")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on a nil server = %v, want nil", err)
	}
}

func TestEmptyBindOpensNoListener(t *testing.T) {
	server := New("", &stubSource{})

	if err := server.Start(); err != nil {
		t.Fatalf("Start() with metrics disabled = %v, want nil", err)
	}
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown() = %v, want nil", err)
		}
	}()
	if server.Enabled() {
		t.Error("an empty bind reported the endpoint as enabled")
	}
	if server.Addr() != "" {
		t.Errorf("Addr() = %q with metrics disabled, want empty", server.Addr())
	}
	// The recorder still exists, so the loops that report into it need no
	// knowledge of whether metrics are served.
	server.Recorder().RecordLoopRun(LoopIssueSync, nil)
}

// A port that is already taken must reach the caller. Discarding it is how a
// deployment ends up believing it exports metrics that nothing serves.
func TestStartReportsABindItCannotTake(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	server := New(held.Addr().String(), &stubSource{})
	err = server.Start()

	if err == nil {
		t.Fatal("Start() on a bound port returned no error")
	}
	if !strings.Contains(err.Error(), held.Addr().String()) {
		t.Errorf("Start() = %v, want the address in the message", err)
	}
}

// End to end over a real socket: the listener starts, serves the series, and
// stops when asked.
func TestListenerServesAndShutsDown(t *testing.T) {
	server := New("127.0.0.1:0", &stubSource{snapshot: Snapshot{QueueDepth: 9}})
	if err := server.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	url := "http://" + server.Addr() + "/metrics"

	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, response.StatusCode)
	}
	if got := mustSample(t, string(body), "moirai_queue_depth"); got != 9 {
		t.Errorf("moirai_queue_depth = %v over HTTP, want 9", got)
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Error("the listener still answered after Shutdown")
	}
}

// The unknown bucket absorbs a mislabelled count; it is not a loop, so it has
// no last-success age. Exporting one would publish a series that grows forever
// by construction and would fire any age alert written against the family.
func TestUnknownLoopHasNoAgeSeries(t *testing.T) {
	body := scrape(t, New("", &stubSource{}))

	if value, exported := sample(body, `moirai_orchestrator_loop_last_success_age_seconds{loop="unknown"}`); exported {
		t.Errorf("age for the unknown loop = %v, want the series to be absent", value)
	}
	if _, exported := sample(body, `moirai_orchestrator_loop_runs_total{loop="unknown",result="success"}`); !exported {
		t.Error("the unknown bucket lost its counter as well as its age")
	}
}
