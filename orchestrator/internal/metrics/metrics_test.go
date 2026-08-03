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
	"sync"
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

// sample returns one exported series' value as written, and reports whether it
// was exported at all. Absence is a meaningful outcome here, not a test
// failure, so it is returned rather than raised — and the value is returned
// unparsed so that "absent" and "exported as something unparseable" stay
// different answers.
func sample(body, series string) (string, bool) {
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		return value, true
	}
	return "", false
}

func mustSample(t *testing.T, body, series string) float64 {
	t.Helper()
	value, exported := sample(body, series)
	if !exported {
		t.Fatalf("%s was not exported; body:\n%s", series, body)
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%s was exported as %q, which is not a number", series, value)
	}
	return parsed
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

	// A client of its own, with keep-alives off: the shared default transport
	// would let a pooled connection from another test decide whether the
	// post-shutdown request below is refused or merely fails on write.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(url)
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
	if _, err := client.Get(url); err == nil {
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

// A loop that has never run at all is still healthy from process start: an
// alert on staleness must not fire the instant the process boots, before the
// first tick has even had a chance to land.
func TestLoopStatusesReportHealthyFromProcessStart(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()

	healthy, statuses := recorder.Ready()
	if !healthy {
		t.Fatalf("Ready() = false at process start, want true; statuses: %+v", statuses)
	}
	for _, status := range statuses {
		if !status.Healthy {
			t.Errorf("loop %q reported unhealthy at process start", status.Name)
		}
		if !status.LastSuccess.Equal(now) {
			t.Errorf("loop %q LastSuccess = %v, want %v (seeded at construction)", status.Name, status.LastSuccess, now)
		}
	}
}

// The crux of the acceptance criteria: a loop that stops succeeding must be
// reported unhealthy once its last success ages past its own staleness
// threshold, and healthy again the moment it recovers.
func TestLoopStatusesReportUnhealthyOnceStale(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()
	// Every loop succeeds once at t=0, so only recovery_sweep is left to go
	// stale below -- the other three are re-recorded as time passes to isolate
	// the one loop under test.
	for _, loop := range scheduledLoops {
		recorder.RecordLoopRun(loop, nil)
	}
	recoverySweepStatus := func() LoopStatus {
		for _, status := range recorder.LoopStatuses() {
			if status.Name == LoopRecoverySweep {
				return status
			}
		}
		t.Fatal("recovery_sweep missing from LoopStatuses()")
		return LoopStatus{}
	}

	// recovery_sweep's interval is 30s; 5x is 150s, comfortably above the floor.
	now = now.Add(100 * time.Second)
	recorder.RecordLoopRun(LoopScheduler, nil)
	recorder.RecordLoopRun(LoopWorkflowObserver, nil)
	recorder.RecordLoopRun(LoopIssueSync, nil)
	if status := recoverySweepStatus(); !status.Healthy {
		t.Fatalf("recovery_sweep = unhealthy at 100s (threshold 150s), want healthy; status: %+v", status)
	}

	now = now.Add(100 * time.Second) // 200s since recovery_sweep's last success
	recorder.RecordLoopRun(LoopScheduler, nil)
	recorder.RecordLoopRun(LoopWorkflowObserver, nil)
	recorder.RecordLoopRun(LoopIssueSync, nil)
	if status := recoverySweepStatus(); status.Healthy {
		t.Fatal("recovery_sweep = healthy at 200s (threshold 150s), want unhealthy: it has been stalled for 50s past its own budget")
	}
	if healthy, statuses := recorder.Ready(); healthy {
		t.Fatalf("Ready() = true with recovery_sweep stalled, want false; statuses: %+v", statuses)
	}

	// A subsequent success clears the staleness immediately.
	recorder.RecordLoopRun(LoopRecoverySweep, nil)
	if status := recoverySweepStatus(); !status.Healthy {
		t.Fatalf("recovery_sweep = unhealthy right after a fresh success, want healthy; status: %+v", status)
	}
	if healthy, statuses := recorder.Ready(); !healthy {
		t.Fatalf("Ready() = false once every loop has succeeded recently, want true; statuses: %+v", statuses)
	}
}

// The scheduler tick runs every second; 5x that is only 5s, which a single
// slow database round trip could exceed without the loop actually having
// stopped. The floor exists precisely to absorb that.
func TestLoopStalenessFloorProtectsFastLoops(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()
	recorder.RecordLoopRun(LoopScheduler, nil)

	now = now.Add(10 * time.Second) // past 5x1s=5s, well under the 30s floor
	healthy, statuses := recorder.Ready()
	if !healthy {
		t.Fatalf("Ready() = false at 10s for a 1s-interval loop, want true (floored at 30s); statuses: %+v", statuses)
	}

	now = now.Add(25 * time.Second) // 35s total, past the 30s floor
	if healthy, statuses := recorder.Ready(); healthy {
		t.Fatalf("Ready() = true at 35s for a 1s-interval loop, want false; statuses: %+v", statuses)
	}
}

// A loop's last error survives its next success: an operator looking at a
// currently-healthy loop should still be able to see that it flaked earlier,
// not have the evidence erased the moment it recovers.
func TestLastErrorSurvivesASubsequentSuccess(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()

	recorder.RecordLoopRun(LoopIssueSync, errors.New("gh: rate limited"))
	now = now.Add(time.Second)
	recorder.RecordLoopRun(LoopIssueSync, nil)

	_, statuses := recorder.Ready()
	for _, status := range statuses {
		if status.Name != LoopIssueSync {
			continue
		}
		if status.LastError != "gh: rate limited" {
			t.Errorf("issue_sync LastError = %q, want the earlier failure preserved", status.LastError)
		}
		if !status.Healthy {
			t.Error("issue_sync reported unhealthy right after a fresh success")
		}
		return
	}
	t.Fatal("issue_sync missing from Ready()'s statuses")
}

// SetLoopInterval is how main.go tells the recorder issue sync's real,
// operator-configurable interval; before it is called the recorder must still
// have a sane default rather than treating the loop as having no budget at
// all.
func TestSetLoopIntervalChangesTheStalenessThreshold(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("", &stubSource{}, func() time.Time { return now })
	recorder := server.Recorder()
	recorder.RecordLoopRun(LoopIssueSync, nil)
	recorder.SetLoopInterval(LoopIssueSync, time.Second) // 5x1s=5s, floored at 30s

	now = now.Add(45 * time.Second)
	if healthy, statuses := recorder.Ready(); healthy {
		t.Fatalf("Ready() = true at 45s after narrowing issue_sync's interval to 1s, want false; statuses: %+v", statuses)
	}
}

// The whole point of /readyz: it is the one channel a *separate* healthcheck
// process has into this process's in-memory loop state, since it cannot read
// this process's memory directly. It must answer 503 once a loop is stale, and
// 200 while every loop is within budget.
func TestReadyzReflectsLoopHealth(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	server := NewWithClock("127.0.0.1:0", &stubSource{}, func() time.Time { return now })
	if err := server.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	recorder := server.Recorder()
	recorder.RecordLoopRun(LoopRecoverySweep, nil)
	recorder.RecordLoopRun(LoopIssueSync, nil)
	recorder.RecordLoopRun(LoopScheduler, nil)
	recorder.RecordLoopRun(LoopWorkflowObserver, nil)

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + server.Addr() + "/readyz"

	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d while every loop is fresh, want 200", url, response.StatusCode)
	}

	// Age recovery_sweep (30s interval, 150s threshold) past its budget.
	now = now.Add(200 * time.Second)
	response, err = client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET %s = %d once recovery_sweep is stale, want 503; body: %s", url, response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"recovery_sweep"`) {
		t.Errorf("/readyz body does not name the stalled loop; body: %s", body)
	}
	if !strings.Contains(string(body), `"healthy":false`) {
		t.Errorf("/readyz body does not report overall unhealthy; body: %s", body)
	}
}

// The endpoint reads the database on every request and the pool it reads
// through is the one the scheduler and the runner streams use, so the number of
// scrapes that may be in flight at once is bounded. Beyond the bound the
// endpoint answers 503 rather than queueing another database read.
func TestConcurrentScrapesAreBounded(t *testing.T) {
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	entered := make(chan struct{}, 8)
	server := New("127.0.0.1:0", blockingSource{entered: entered, release: release})
	if err := server.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	defer func() {
		releaseAll()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://" + server.Addr() + "/metrics"

	// Fill the allowance, and wait until both requests are actually inside the
	// collector rather than merely dispatched.
	codes := make(chan int, 3)
	for range 2 {
		go func() {
			response, err := client.Get(url)
			if err != nil {
				codes <- 0
				return
			}
			response.Body.Close()
			codes <- response.StatusCode
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("a scrape never reached the source")
		}
	}

	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("third scrape: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("third concurrent scrape = %d, want 503: an unauthenticated endpoint must not be able to drain the pool", response.StatusCode)
	}

	releaseAll()
	for range 2 {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("scrape within the allowance = %d, want 200", code)
		}
	}
}

// blockingSource holds each read open until it is released, so a test can have
// two scrapes genuinely in flight at once.
type blockingSource struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (s blockingSource) MetricsSnapshot(ctx context.Context) (Snapshot, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	return Snapshot{QueueDepth: 1}, nil
}
