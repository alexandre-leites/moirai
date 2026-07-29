package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/loop-engineering/runner/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newMeteredReporter builds a reporter whose transport always fails, so emitted
// events stay queued and the buffer can be driven to its limit, and points it
// at a recorder of the test's own rather than the process-wide one.
func newMeteredReporter(t *testing.T, maxPending int) (*EventReporter, *metrics.Recorder) {
	t.Helper()
	reporter, err := NewEventReporter(&eventClient{err: errors.New("control stream is down")}, maxPending, nil, "")
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	recorder := metrics.NewRecorder(nil)
	reporter.Metrics = recorder
	return reporter, recorder
}

func beginMeteredLease(t *testing.T, reporter *EventReporter) Lease {
	t.Helper()
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	return lease
}

// queueLog emits a log event that the failing transport leaves in the queue.
func queueLog(t *testing.T, reporter *EventReporter, lease Lease, message string) {
	t.Helper()
	sequence, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": message})
	if sequence == 0 {
		t.Fatalf("Emit(log) was not queued: %v", err)
	}
}

// TestDroppedLogEventIncrementsTheDroppedEventCounter is the acceptance test
// for the counter itself: a log event the bounded buffer has no room for is
// lost, and until now that loss was visible only in a log line. It goes through
// EmitLog, which is what the runner's log forwarding actually calls — nothing
// retries log output, so EmitLog is where the loss becomes final and is counted.
func TestDroppedLogEventIncrementsTheDroppedEventCounter(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 2)
	lease := beginMeteredLease(t, reporter)
	queueLog(t, reporter, lease, "first")
	queueLog(t, reporter, lease, "second")

	if _, err := reporter.EmitLog(lease.JobID, lease.Generation, "third"); !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("EmitLog() into a full buffer error = %v, want ErrEventBufferFull", err)
	}

	body := scrapeRecorder(t, recorder)
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="buffer_full"}`); value != 1 {
		t.Errorf("buffer_full drops = %v, want 1", value)
	}
	if value := metricValue(t, body, "moirai_runner_pending_events"); value != 2 {
		t.Errorf("pending events = %v, want 2", value)
	}
}

// A log message longer than the chunk limit is emitted as several events. When
// one chunk is rejected the rest are never attempted, so they are lost too;
// counting only the rejected chunk would understate the loss exactly when a
// chatty agent is what is overrunning the buffer.
func TestDroppedMultiChunkLogCountsEveryLostChunk(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 2)
	lease := beginMeteredLease(t, reporter)
	queueLog(t, reporter, lease, "first")
	queueLog(t, reporter, lease, "second")

	// Four chunks' worth of output, none of which can be queued.
	message := strings.Repeat("x", 3*maxLogChunkBytes+1)
	chunks := len(splitUTF8Chunks(message, maxLogChunkBytes))
	if chunks != 4 {
		t.Fatalf("test message split into %d chunks, want 4", chunks)
	}
	if _, err := reporter.EmitLog(lease.JobID, lease.Generation, message); !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("EmitLog() error = %v, want ErrEventBufferFull", err)
	}

	if value := metricValue(t, scrapeRecorder(t, recorder), `moirai_runner_events_dropped_total{reason="buffer_full"}`); value != float64(chunks) {
		t.Errorf("buffer_full drops = %v, want %d", value, chunks)
	}
}

// Emit does not count its own rejections: a rejected event is not necessarily a
// lost one, because ControlLoop.emitEvent re-emits a terminal event with a
// reduced payload. Counting here would book a drop against a run that went on
// to report its outcome.
func TestEmitDoesNotCountRejectionsTheCallerMayRetry(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 2)
	lease := beginMeteredLease(t, reporter)
	queueLog(t, reporter, lease, "first")
	queueLog(t, reporter, lease, "second")

	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "third"}); !errors.Is(err, ErrEventBufferFull) {
		t.Fatalf("Emit() into a full buffer error = %v, want ErrEventBufferFull", err)
	}
	oversized := strings.Repeat("x", maxExecutionEventPayloadBytes+1)
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"message": oversized}); !errors.Is(err, ErrInvalidExecutionEvent) {
		t.Fatalf("Emit() with an oversized payload error = %v, want ErrInvalidExecutionEvent", err)
	}
	if _, err := reporter.Emit("job-unknown", lease.Generation, "log", nil); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("Emit() without a lease error = %v, want ErrNoActiveEventLease", err)
	}

	body := scrapeRecorder(t, recorder)
	for _, reason := range []string{"buffer_full", "invalid", "no_lease", "unknown"} {
		series := `moirai_runner_events_dropped_total{reason="` + reason + `"}`
		if value := metricValue(t, body, series); value != 0 {
			t.Errorf("%s = %v, want 0 — Emit counted a rejection its caller may still recover from", series, value)
		}
	}
}

func TestDropReasonClassifiesEveryRejection(t *testing.T) {
	for _, testCase := range []struct {
		err    error
		reason string
	}{
		{ErrEventBufferFull, metrics.DropBufferFull},
		{fmt.Errorf("wrapped: %w", ErrEventBufferFull), metrics.DropBufferFull},
		{ErrNoActiveEventLease, metrics.DropNoLease},
		{ErrStaleEventLease, metrics.DropNoLease},
		{ErrEventOutboxUnavailable, metrics.DropPersistFailed},
		{ErrInvalidExecutionEvent, metrics.DropInvalid},
		{errors.New("the control stream is down"), metrics.DropUnknown},
	} {
		if reason := DropReason(testCase.err); reason != testCase.reason {
			t.Errorf("DropReason(%v) = %q, want %q", testCase.err, reason, testCase.reason)
		}
	}
}

func TestEvictedEventIncrementsTheDroppedEventCounter(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 2)
	lease := beginMeteredLease(t, reporter)
	queueLog(t, reporter, lease, "first")
	queueLog(t, reporter, lease, "second")

	// A terminal event outranks queued log output and takes its slot, so the
	// evicted log is a drop even though the emit itself succeeded.
	if sequence, _ := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"}); sequence == 0 {
		t.Fatal("terminal event was not queued")
	}

	body := scrapeRecorder(t, recorder)
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="evicted"}`); value != 1 {
		t.Errorf("evicted drops = %v, want 1", value)
	}
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="buffer_full"}`); value != 0 {
		t.Errorf("buffer_full drops = %v, want 0", value)
	}
}

func TestAbandonedLeaseCountsEveryDiscardedEvent(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 8)
	lease := beginMeteredLease(t, reporter)
	queueLog(t, reporter, lease, "first")
	queueLog(t, reporter, lease, "second")
	if sequence, _ := reporter.Emit(lease.JobID, lease.Generation, "completed", map[string]any{"status": "completed"}); sequence == 0 {
		t.Fatal("terminal event was not queued")
	}

	if !reporter.Abandon(lease.JobID, lease.Generation) {
		t.Fatal("Abandon() did not release the lease")
	}

	body := scrapeRecorder(t, recorder)
	// The two log events are discarded; the terminal event is kept, which is
	// exactly why the counter is taken from the queue length rather than
	// assumed to be everything the job had queued.
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="lease_expired"}`); value != 2 {
		t.Errorf("lease_expired drops = %v, want 2", value)
	}
	if value := metricValue(t, body, "moirai_runner_pending_events"); value != 1 {
		t.Errorf("pending events after abandon = %v, want 1", value)
	}
}

// A rejected log event is lost for good, and EmitLog labels each loss with the
// reason the reporter refused it.
func TestLostLogEventsAreCountedByReason(t *testing.T) {
	reporter, recorder := newMeteredReporter(t, 4)
	lease := beginMeteredLease(t, reporter)

	if _, err := reporter.EmitLog("job-unknown", lease.Generation, "orphan"); !errors.Is(err, ErrNoActiveEventLease) {
		t.Fatalf("EmitLog() without a lease error = %v, want ErrNoActiveEventLease", err)
	}
	if _, err := reporter.EmitLog(lease.JobID, lease.Generation+1, "stale"); !errors.Is(err, ErrStaleEventLease) {
		t.Fatalf("EmitLog() at a stale generation error = %v, want ErrStaleEventLease", err)
	}

	// A chunk that is queued but whose delivery fails is not lost — the outbox
	// retries it — but the chunks after it are never attempted and are. The
	// transport failure is outside the counter's vocabulary, so it is counted
	// as `unknown` rather than mislabelled.
	if _, err := reporter.EmitLog(lease.JobID, lease.Generation, strings.Repeat("y", maxLogChunkBytes+1)); err == nil {
		t.Fatal("EmitLog() over a failing transport was expected to fail")
	}

	body := scrapeRecorder(t, recorder)
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="no_lease"}`); value != 2 {
		t.Errorf("no_lease drops = %v, want 2", value)
	}
	if value := metricValue(t, body, `moirai_runner_events_dropped_total{reason="unknown"}`); value != 1 {
		t.Errorf("unknown drops = %v, want 1 (the second chunk, never attempted)", value)
	}
}

// The gauge is republished from the queue itself at every change, so delivery
// has to bring it back down rather than leaving it at its high-water mark.
func TestPendingEventGaugeFollowsTheQueueDown(t *testing.T) {
	client := &eventClient{}
	reporter, err := NewEventReporter(client, 4, nil, "")
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	recorder := metrics.NewRecorder(nil)
	reporter.Metrics = recorder
	lease := beginMeteredLease(t, reporter)

	if _, err := reporter.Emit(lease.JobID, lease.Generation, "log", map[string]any{"message": "delivered"}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	if reporter.Pending() != 0 {
		t.Fatalf("Pending() = %d, want 0", reporter.Pending())
	}
	if value := metricValue(t, scrapeRecorder(t, recorder), "moirai_runner_pending_events"); value != 0 {
		t.Errorf("pending events after delivery = %v, want 0", value)
	}
}

// A reporter without an explicit recorder must still record somewhere the
// metrics server can see, which is the process-wide recorder it serves.
func TestReporterDefaultsToTheProcessWideRecorder(t *testing.T) {
	reporter, err := NewEventReporter(&eventClient{}, 4, nil, "")
	if err != nil {
		t.Fatalf("NewEventReporter() error = %v", err)
	}
	if reporter.recorder() != metrics.Default() {
		t.Error("reporter did not fall back to the process-wide recorder")
	}
	recorder := metrics.NewRecorder(nil)
	reporter.Metrics = recorder
	if reporter.recorder() != recorder {
		t.Error("reporter ignored the recorder it was given")
	}
}

func scrapeRecorder(t *testing.T, recorder *metrics.Recorder) string {
	t.Helper()
	response := httptest.NewRecorder()
	promhttp.HandlerFor(recorder.Registry(), promhttp.HandlerOpts{}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func metricValue(t *testing.T, body, series string) float64 {
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
