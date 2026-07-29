package dispatch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newMeteredLoop builds a control loop reporting into a recorder of the test's
// own, so nothing here depends on — or disturbs — the process-wide recorder.
func newMeteredLoop(t *testing.T, client ControlClient, dispatcher executionDispatcher, now time.Time) (*ControlLoop, *metrics.Recorder) {
	t.Helper()
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	recorder := metrics.NewRecorder(nil)
	loop.UseMetrics(recorder)
	return loop, recorder
}

// leaseJob drives a job from offer to acknowledged lease, which is the point at
// which the loop starts an execution.
func leaseJob(t *testing.T, loop *ControlLoop, offer *runnerv1.JobOffer, now time.Time) {
	t.Helper()
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	acknowledgement := &runnerv1.LeaseAcknowledged{
		JobId:           offer.GetJobId(),
		LeaseGeneration: offer.GetLeaseGeneration(),
		ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli(),
	}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: acknowledgement}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
}

func TestControlLoopCountsExecutionsByOutcome(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		result  Result
		outcome string
	}{
		{name: "completed", result: Result{Status: "completed", Summary: "done"}, outcome: "completed"},
		{name: "failed", result: Result{Status: "failed", Summary: "the build broke"}, outcome: "failed"},
		{name: "blocked", result: Result{Status: "blocked", Summary: "needs a credential"}, outcome: "blocked"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			client := &loopClient{}
			loop, recorder := newMeteredLoop(t, client, &staticDispatcher{result: testCase.result}, now)

			leaseJob(t, loop, loopOffer(t), now)
			waitForEvents(t, client, 2)
			waitForMetric(t, recorder, `moirai_runner_executions_completed_total{outcome="`+testCase.outcome+`"}`, 1)

			body := scrapeLoopRecorder(t, recorder)
			if value := loopMetricValue(t, body, "moirai_runner_executions_started_total"); value != 1 {
				t.Errorf("executions started = %v, want 1", value)
			}
			for _, other := range []string{"completed", "failed", "blocked", "cancelled"} {
				if other == testCase.outcome {
					continue
				}
				series := `moirai_runner_executions_completed_total{outcome="` + other + `"}`
				if value := loopMetricValue(t, body, series); value != 0 {
					t.Errorf("%s = %v, want 0", series, value)
				}
			}
		})
	}
}

// The busy gauge has to follow the same predicate Admit uses, in both
// directions: a runner that stayed "busy" after its execution finished would
// look permanently unavailable to whoever reads the metric.
func TestControlLoopPublishesBusyStateInBothDirections(t *testing.T) {
	now := time.Now()
	client := &loopClient{}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, recorder := newMeteredLoop(t, client, dispatcher, now)

	if value := loopMetricValue(t, scrapeLoopRecorder(t, recorder), "moirai_runner_busy"); value != 0 {
		t.Fatalf("busy before any offer = %v, want 0", value)
	}

	offer := loopOffer(t)
	leaseJob(t, loop, offer, now)
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}
	if value := loopMetricValue(t, scrapeLoopRecorder(t, recorder), "moirai_runner_busy"); value != 1 {
		t.Fatalf("busy while at capacity = %v, want 1", value)
	}

	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Cancel{
		Cancel: &runnerv1.CancelExecution{ExecutionId: "execution-1", LeaseGeneration: offer.GetLeaseGeneration()},
	}}); err != nil {
		t.Fatalf("Handle(cancel) error = %v", err)
	}
	waitForMetric(t, recorder, "moirai_runner_busy", 0)
	waitForMetric(t, recorder, `moirai_runner_executions_completed_total{outcome="cancelled"}`, 1)
}

// The busy gauge is published from several goroutines — every Handle, every
// expiry sweep, and the tail of every execution — so the read of the capacity
// state and the write that reports it are serialized. This drives all of them
// at once and asserts the gauge agrees with the truth once the dust settles;
// under -race it also covers the publishing itself.
func TestControlLoopBusyGaugeConvergesUnderConcurrentPublishers(t *testing.T) {
	const jobs = 20
	now := time.Now()
	client := &loopClient{}
	// Enough capacity for every job to be admitted, so the runner really does
	// pass through "at capacity" on its way back to idle.
	loop, err := NewControlLoopWithCapacity(client, &staticDispatcher{result: Result{Status: "completed", Summary: "done"}},
		func() time.Time { return now }, time.Minute, 15*time.Second, nil, "", jobs)
	if err != nil {
		t.Fatalf("NewControlLoopWithCapacity() error = %v", err)
	}
	recorder := metrics.NewRecorder(nil)
	loop.UseMetrics(recorder)

	var publishers sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					loop.publishBusy()
				}
			}
		}()
	}

	for round := range jobs {
		offer := loopOfferFor(t, fmt.Sprintf("job-%d", round), fmt.Sprintf("execution-%d", round), fmt.Sprintf("project-%d", round))
		leaseJob(t, loop, offer, now)
	}
	waitForEvents(t, client, 2*jobs)
	close(stop)
	publishers.Wait()

	// Every execution has reported its outcome, so the runner is idle; the last
	// publish must say so.
	loop.publishBusy()
	if loop.Busy() {
		t.Fatal("runner is still busy after every execution reported a terminal event")
	}
	if value := loopMetricValue(t, scrapeLoopRecorder(t, recorder), "moirai_runner_busy"); value != 0 {
		t.Errorf("moirai_runner_busy = %v, want 0", value)
	}
	waitForMetric(t, recorder, `moirai_runner_executions_completed_total{outcome="completed"}`, jobs)
	if value := loopMetricValue(t, scrapeLoopRecorder(t, recorder), "moirai_runner_executions_started_total"); value != jobs {
		t.Errorf("executions started = %v, want %d", value, jobs)
	}
}

func TestControlLoopDefaultsToTheProcessWideRecorder(t *testing.T) {
	loop, err := NewControlLoopWithOutbox(&loopClient{}, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	if loop.recorder() != metrics.Default() {
		t.Error("loop did not fall back to the process-wide recorder")
	}

	recorder := metrics.NewRecorder(nil)
	loop.UseMetrics(recorder)
	if loop.recorder() != recorder {
		t.Error("UseMetrics did not point the loop at the recorder")
	}
	// The reporter owns the queue and its drop counters, so UseMetrics has to
	// carry it along or those land somewhere nothing is scraping.
	if loop.Reporter.Metrics != recorder {
		t.Error("UseMetrics did not point the event reporter at the same recorder")
	}
}

// waitForMetric polls a series until it reaches want. Terminal bookkeeping runs
// on the execution's own goroutine, so the value is not visible the instant the
// event that triggered it is.
func waitForMetric(t *testing.T, recorder *metrics.Recorder, series string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last float64
	for time.Now().Before(deadline) {
		last = loopMetricValue(t, scrapeLoopRecorder(t, recorder), series)
		if last == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s = %v, last saw %v", series, want, last)
}

func scrapeLoopRecorder(t *testing.T, recorder *metrics.Recorder) string {
	t.Helper()
	response := httptest.NewRecorder()
	promhttp.HandlerFor(recorder.Registry(), promhttp.HandlerOpts{}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape returned %d: %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func loopMetricValue(t *testing.T, body, series string) float64 {
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
