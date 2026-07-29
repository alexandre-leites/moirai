package metrics

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// testClock is a clock a test advances by hand. The heartbeat-age gauge is
// evaluated during a scrape, which happens on the test's own goroutine, but the
// clock is mutex-guarded so this holds under -race even if that ever changes.
type testClock struct {
	mu      sync.Mutex
	current time.Time
}

func newTestClock() *testClock {
	return &testClock{current: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

// TestHeartbeatAgeGaugeAgesWithTheInjectedClock is the regression test for the
// placeholder this package used to export: a gauge set to zero at construction
// and never written again, which reported "the heartbeat just happened" forever
// (issue #124). The age is now computed at scrape time from the recorder's
// clock, so advancing that clock is enough to observe it grow — no wall-clock
// sleep, and no dependence on how long the test itself takes.
func TestHeartbeatAgeGaugeAgesWithTheInjectedClock(t *testing.T) {
	clock := newTestClock()
	recorder := NewRecorder(clock.Now)

	recorder.MarkHeartbeat()
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 0 {
		t.Fatalf("heartbeat age immediately after a heartbeat = %v, want 0", age)
	}

	clock.Advance(30 * time.Second)
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 30 {
		t.Fatalf("heartbeat age 30s after a heartbeat = %v, want 30", age)
	}

	clock.Advance(90 * time.Second)
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 120 {
		t.Fatalf("heartbeat age 120s after a heartbeat = %v, want 120", age)
	}

	recorder.MarkHeartbeat()
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 0 {
		t.Fatalf("heartbeat age after a fresh heartbeat = %v, want 0", age)
	}
}

// A runner that never reaches the orchestrator is the case an age alert most
// needs to catch, so the gauge counts from process start rather than sitting at
// zero until the first heartbeat that may never arrive.
func TestHeartbeatAgeCountsFromStartBeforeAnyHeartbeat(t *testing.T) {
	clock := newTestClock()
	recorder := NewRecorder(clock.Now)

	clock.Advance(45 * time.Second)
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 45 {
		t.Fatalf("heartbeat age without any heartbeat = %v, want 45", age)
	}
}

func TestHeartbeatAgeIsNeverNegative(t *testing.T) {
	clock := newTestClock()
	recorder := NewRecorder(clock.Now)
	recorder.MarkHeartbeat()

	clock.Advance(-time.Hour)
	if age := sampleValue(t, scrape(t, recorder), "moirai_runner_heartbeat_age_seconds"); age != 0 {
		t.Fatalf("heartbeat age with a clock that moved backwards = %v, want 0", age)
	}
}

// The runner has no database and cannot populate orchestrator-owned state, so
// it must not export it at all.
func TestOrchestratorOwnedPlaceholdersAreNotExported(t *testing.T) {
	body := scrape(t, NewRecorder(newTestClock().Now))
	for _, name := range []string{"moirai_queue_depth", "moirai_active_workflow_count"} {
		if strings.Contains(body, name) {
			t.Errorf("runner still exports orchestrator-owned %s:\n%s", name, body)
		}
	}
}

func TestExecutionAndEventSeriesExistAtZeroBeforeAnythingHappens(t *testing.T) {
	body := scrape(t, NewRecorder(newTestClock().Now))
	for _, series := range []string{
		"moirai_runner_executions_started_total",
		`moirai_runner_executions_completed_total{outcome="completed"}`,
		`moirai_runner_executions_completed_total{outcome="failed"}`,
		`moirai_runner_executions_completed_total{outcome="blocked"}`,
		`moirai_runner_executions_completed_total{outcome="cancelled"}`,
		`moirai_runner_events_dropped_total{reason="buffer_full"}`,
		`moirai_runner_events_dropped_total{reason="evicted"}`,
		`moirai_runner_events_dropped_total{reason="lease_expired"}`,
		"moirai_runner_busy",
		"moirai_runner_pending_events",
	} {
		if value := sampleValue(t, body, series); value != 0 {
			t.Errorf("%s = %v, want 0", series, value)
		}
	}
}

func TestRunnerOwnedValuesAreRecorded(t *testing.T) {
	recorder := NewRecorder(newTestClock().Now)
	recorder.RecordExecutionStarted()
	recorder.RecordExecutionStarted()
	recorder.RecordExecutionCompleted(OutcomeCompleted)
	recorder.RecordExecutionCompleted(OutcomeFailed)
	recorder.SetBusy(true)
	recorder.SetPendingEvents(7)
	recorder.RecordEventDropped(DropBufferFull)
	recorder.RecordEventsDropped(DropEvicted, 3)

	body := scrape(t, recorder)
	for series, want := range map[string]float64{
		"moirai_runner_executions_started_total":                        2,
		`moirai_runner_executions_completed_total{outcome="completed"}`: 1,
		`moirai_runner_executions_completed_total{outcome="failed"}`:    1,
		"moirai_runner_busy":                                       1,
		"moirai_runner_pending_events":                             7,
		`moirai_runner_events_dropped_total{reason="buffer_full"}`: 1,
		`moirai_runner_events_dropped_total{reason="evicted"}`:     3,
	} {
		if value := sampleValue(t, body, series); value != want {
			t.Errorf("%s = %v, want %v", series, value, want)
		}
	}

	recorder.SetBusy(false)
	if value := sampleValue(t, scrape(t, recorder), "moirai_runner_busy"); value != 0 {
		t.Errorf("moirai_runner_busy after SetBusy(false) = %v, want 0", value)
	}
}

// An outcome or reason outside the known set must not mint a new time series:
// both labels come from strings assembled elsewhere in the runner, and an
// unbounded label is how a metrics endpoint turns into an outage.
func TestUnknownLabelValuesFoldIntoABoundedBucket(t *testing.T) {
	recorder := NewRecorder(newTestClock().Now)
	before := seriesCount(scrape(t, recorder))

	recorder.RecordExecutionCompleted("something-new")
	recorder.RecordEventsDropped("something-else", 2)

	body := scrape(t, recorder)
	if value := sampleValue(t, body, `moirai_runner_executions_completed_total{outcome="unknown"}`); value != 1 {
		t.Errorf("unknown outcome bucket = %v, want 1", value)
	}
	if value := sampleValue(t, body, `moirai_runner_events_dropped_total{reason="unknown"}`); value != 2 {
		t.Errorf("unknown drop-reason bucket = %v, want 2", value)
	}
	if after := seriesCount(body); after != before {
		t.Errorf("series count grew from %d to %d; label cardinality is not bounded", before, after)
	}
}

func TestRecordEventsDroppedIgnoresNonPositiveCounts(t *testing.T) {
	recorder := NewRecorder(newTestClock().Now)
	recorder.RecordEventsDropped(DropLeaseExpired, 0)
	recorder.RecordEventsDropped(DropLeaseExpired, -4)
	if value := sampleValue(t, scrape(t, recorder), `moirai_runner_events_dropped_total{reason="lease_expired"}`); value != 0 {
		t.Errorf("lease_expired drops = %v, want 0", value)
	}
}

func TestServerServesItsRecorderAndForwardsHeartbeats(t *testing.T) {
	clock := newTestClock()
	recorder := NewRecorder(clock.Now)
	server := NewWithRecorder("127.0.0.1:0", recorder)

	server.MarkHeartbeat()
	clock.Advance(12 * time.Second)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", response.Code)
	}
	if age := sampleValue(t, response.Body.String(), "moirai_runner_heartbeat_age_seconds"); age != 12 {
		t.Fatalf("heartbeat age through the server handler = %v, want 12", age)
	}
	if server.Recorder() != recorder {
		t.Error("server did not retain the recorder it was given")
	}
}

func TestNewUsesTheProcessWideRecorder(t *testing.T) {
	if New("127.0.0.1:0").Recorder() != Default() {
		t.Error("New must serve the process-wide recorder the rest of the runner records into")
	}
	if NewWithRecorder("127.0.0.1:0", nil).Recorder() != Default() {
		t.Error("a nil recorder must fall back to the process-wide one")
	}
}

func TestNilRecorderMethodsAreSafe(t *testing.T) {
	var recorder *Recorder
	recorder.MarkHeartbeat()
	recorder.SetBusy(true)
	recorder.SetPendingEvents(3)
	recorder.RecordExecutionStarted()
	recorder.RecordExecutionCompleted(OutcomeFailed)
	recorder.RecordEventDropped(DropInvalid)
	if age := recorder.HeartbeatAgeSeconds(); age != 0 {
		t.Errorf("nil recorder heartbeat age = %v, want 0", age)
	}
}

// scrape renders a recorder exactly as a Prometheus server would read it.
func scrape(t *testing.T, recorder *Recorder) string {
	t.Helper()
	response := httptest.NewRecorder()
	promhttp.HandlerFor(recorder.Registry(), promhttp.HandlerOpts{}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

// sampleValue reads one series — name plus its exact label set — out of the
// text exposition format.
func sampleValue(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, raw, ok := strings.Cut(line, " ")
		if !ok || name != series {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		return value
	}
	t.Fatalf("series %q is not exported:\n%s", series, body)
	return 0
}

func seriesCount(body string) int {
	count := 0
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			count++
		}
	}
	return count
}
