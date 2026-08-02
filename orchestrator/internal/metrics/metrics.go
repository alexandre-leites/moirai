// Package metrics publishes the Prometheus series the orchestrator owns.
//
// Queue depth, active workflow counts and the fleet-wide runner heartbeat age
// are derived from the orchestrator's database, so the orchestrator is the only
// service that can export them: the API has no database access (PROJECT.md,
// "Service boundaries") and a runner sees only itself. Issue #124 removed the
// constant-zero copies the API and the runner used to register — a gauge that
// never moves cannot raise an alert, and reads as healthy while the system
// burns — and this package is where those series come back, computed at scrape
// time from the same query the console's scheduler-metrics RPC runs.
//
// Two rules follow from that history and are load-bearing here:
//
//   - Every state series is read at scrape time, never cached and never
//     pre-set. A value is only true at the instant it is read.
//   - A read that fails omits the series rather than reporting zero. "The
//     database did not answer" and "the queue is empty" must not look the same
//     to an alert.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeTimeout bounds the database read a scrape performs. Prometheus scrapes
// on an interval of its own and gets no cancellation signal through to a
// collector, so the bound has to be here: without it a stuck query would pin a
// pool connection for as long as the database took to notice.
const scrapeTimeout = 5 * time.Second

// Reconciliation loops whose outcome is exported. The set is closed so the
// `loop` label cannot grow: a name outside it is folded into LoopUnknown rather
// than minting a series.
const (
	LoopRecoverySweep = "recovery_sweep"
	LoopIssueSync     = "issue_sync"
	LoopUnknown       = "unknown"
)

const (
	resultSuccess = "success"
	resultFailure = "failure"
)

// scheduledLoops are the loops the process actually runs, and therefore the
// only ones with a meaningful "time since last success". loopNames adds the
// unknown bucket, which exists to absorb a mislabelled count: exporting an age
// for it would publish a series that grows forever by construction, since
// nothing ever succeeds under that name.
var (
	scheduledLoops = []string{LoopRecoverySweep, LoopIssueSync}
	loopNames      = append(append([]string{}, scheduledLoops...), LoopUnknown)
)

// Snapshot is one reading of the orchestrator-owned state, taken from a single
// database query.
type Snapshot struct {
	// QueueDepth counts eligible open issues in enabled projects — the
	// scheduler's candidate set. An issue stays eligible while its workflow
	// runs (it is parked only when the run delivers or ends), so this includes
	// work already in flight, at most one issue per project.
	QueueDepth int64
	// ActiveWorkflows counts workflow runs that have not reached a terminal
	// status.
	ActiveWorkflows int64
	// ScheduledJobs counts jobs offered to, being prepared by, or running on a
	// runner.
	ScheduledJobs int64
	// EnabledRunners counts runners an operator has not disabled or revoked.
	// It is what makes an absent heartbeat age readable: no age and no enabled
	// runner is an idle deployment, no age with runners enabled would be a bug.
	EnabledRunners int64
	// OldestHeartbeatAge is how long ago the *least* recently seen enabled
	// runner checked in — the fleet-wide worst case, which is the number an
	// alert on a disconnected fleet needs. HeartbeatKnown is false when no
	// enabled runner exists, and the series is then omitted rather than
	// exported as zero.
	OldestHeartbeatAge time.Duration
	HeartbeatKnown     bool
}

// Source reads a Snapshot from the orchestrator's database. The server
// implements it; tests substitute their own.
type Source interface {
	MetricsSnapshot(ctx context.Context) (Snapshot, error)
}

var (
	queueDepthDesc = prometheus.NewDesc(
		"moirai_queue_depth",
		"Eligible open issues in enabled projects: the scheduler's candidate set. An issue stays eligible while its workflow runs, so this includes work already in flight.",
		nil, nil)
	activeWorkflowsDesc = prometheus.NewDesc(
		"moirai_active_workflows",
		"Workflow runs that have not reached a terminal status.",
		nil, nil)
	scheduledJobsDesc = prometheus.NewDesc(
		"moirai_scheduled_jobs",
		"Jobs offered to, being prepared by, or running on a runner.",
		nil, nil)
	enabledRunnersDesc = prometheus.NewDesc(
		"moirai_enabled_runners",
		"Runners an operator has neither disabled nor revoked.",
		nil, nil)
	heartbeatAgeDesc = prometheus.NewDesc(
		"moirai_runner_heartbeat_age_seconds",
		"Seconds since the least recently seen enabled runner last checked in. Absent when no runner is enabled.",
		nil, nil)
	loopSuccessAgeDesc = prometheus.NewDesc(
		"moirai_orchestrator_loop_last_success_age_seconds",
		"Seconds since a reconciliation loop last completed without error. Counts from process start until its first success.",
		[]string{"loop"}, nil)
	scrapeErrorsDesc = prometheus.NewDesc(
		"moirai_orchestrator_metrics_scrape_errors_total",
		"Scrapes that could not read orchestrator state and therefore omitted the state series instead of reporting zero.",
		nil, nil)
)

// Recorder holds the process-side state the collector exports: the outcome of
// each reconciliation loop, and the scrapes that could not read the database.
// Its methods are safe for concurrent use and safe on a nil receiver, so a
// caller wired before the metrics server exists still compiles and runs.
type Recorder struct {
	now func() time.Time

	// scrapeErrors is a plain counter emitted by the collector rather than a
	// registered prometheus.Counter, because a registered one is gathered
	// concurrently with the collector that increments it: the scrape that
	// failed could report the count from before its own failure. Emitting it
	// from the same Collect call, after the read, makes each response
	// self-consistent.
	scrapeErrors atomic.Int64
	loopRuns     *prometheus.CounterVec
	// lastSuccess holds the clock reading at each loop's last successful run,
	// seeded with the recorder's construction time so a loop that has never
	// succeeded ages from process start — exactly the condition an age alert
	// has to catch. It stores the time.Time rather than a Unix stamp so the
	// monotonic reading survives and a wall-clock step cannot report a loop as
	// having just succeeded.
	lastSuccess map[string]*atomic.Pointer[time.Time]
}

func newRecorder(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	recorder := &Recorder{
		now: now,
		loopRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "moirai_orchestrator_loop_runs_total",
			Help: "Reconciliation loop passes, by loop and result.",
		}, []string{"loop", "result"}),
		lastSuccess: make(map[string]*atomic.Pointer[time.Time], len(loopNames)),
	}
	started := now()
	// Every label child is materialised now, so each series exists at zero from
	// the first scrape: a counter that only appears once it is non-zero cannot
	// be alerted on with rate() until the first failure has already happened.
	for _, loop := range loopNames {
		for _, result := range []string{resultSuccess, resultFailure} {
			recorder.loopRuns.WithLabelValues(loop, result)
		}
		seed := started
		pointer := &atomic.Pointer[time.Time]{}
		pointer.Store(&seed)
		recorder.lastSuccess[loop] = pointer
	}
	return recorder
}

// RecordLoopRun counts one pass of a reconciliation loop. A loop name outside
// the known set is counted as LoopUnknown, bounding the label cardinality.
func (r *Recorder) RecordLoopRun(loop string, err error) {
	if r == nil {
		return
	}
	last, known := r.lastSuccess[loop]
	if !known {
		loop, last = LoopUnknown, r.lastSuccess[LoopUnknown]
	}
	if err != nil {
		r.loopRuns.WithLabelValues(loop, resultFailure).Inc()
		return
	}
	r.loopRuns.WithLabelValues(loop, resultSuccess).Inc()
	at := r.now()
	last.Store(&at)
}

// loopSuccessAge reports the seconds since a loop last succeeded. Both readings
// come from the same clock, so the subtraction is monotonic; the clamp covers a
// clock that genuinely runs backwards, since no consumer of an age has a
// meaning for a negative one.
func (r *Recorder) loopSuccessAge(loop string) float64 {
	stored, known := r.lastSuccess[loop]
	if !known {
		return 0
	}
	last := stored.Load()
	if last == nil {
		return 0
	}
	age := r.now().Sub(*last)
	if age < 0 {
		return 0
	}
	return age.Seconds()
}

// collector reads the database at scrape time and emits the state series. It is
// a collector rather than a set of gauges refreshed by a timer because a cached
// gauge reports whatever the last tick left behind, and there is no scrape-side
// signal that the tick stopped happening: the previous orchestrator refreshed
// four gauges from a snapshot loop that raised on every tick, so all four
// served their initial zero for the life of the process (issue #195). With a
// collector there is no tick to fail — a read that fails is visible as a
// missing series and a scrape-error count on the very next scrape.
type collector struct {
	source   Source
	recorder *Recorder
	timeout  time.Duration
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- queueDepthDesc
	ch <- activeWorkflowsDesc
	ch <- scheduledJobsDesc
	ch <- enabledRunnersDesc
	ch <- heartbeatAgeDesc
	ch <- loopSuccessAgeDesc
	ch <- scrapeErrorsDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	for _, loop := range scheduledLoops {
		ch <- prometheus.MustNewConstMetric(loopSuccessAgeDesc, prometheus.GaugeValue, c.recorder.loopSuccessAge(loop), loop)
	}
	c.collectState(ch)
	// Emitted last, so a scrape that just failed reports its own failure rather
	// than the count from before it.
	ch <- prometheus.MustNewConstMetric(scrapeErrorsDesc, prometheus.CounterValue, float64(c.recorder.scrapeErrors.Load()))
}

// collectState emits the database-derived series, or none of them.
func (c *collector) collectState(ch chan<- prometheus.Metric) {
	// The registry already recovers a panicking collector (safeCollect), so
	// this is not what keeps the process alive. It is what keeps the *rest of
	// the response* alive: the registry turns the panic into a gather error,
	// and promhttp's default error handling then answers 500 and serves no
	// body at all — losing the loop series and the scrape-error counter, which
	// are precisely the series that would tell an operator something is wrong.
	// Contained here, one bad read costs the state series and nothing else.
	defer func() {
		if recovered := recover(); recovered != nil {
			c.recorder.scrapeErrors.Add(1)
			slog.Error("metrics scrape panicked", "panic", recovered)
		}
	}()
	if c.source == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	snapshot, err := c.source.MetricsSnapshot(ctx)
	if err != nil {
		// Omitted, not zeroed: a failed read says nothing about the queue, and
		// a zero would claim it is empty. The error counter is what makes the
		// gap visible.
		c.recorder.scrapeErrors.Add(1)
		slog.Warn("metrics scrape failed", "error", err)
		return
	}
	ch <- prometheus.MustNewConstMetric(queueDepthDesc, prometheus.GaugeValue, float64(snapshot.QueueDepth))
	ch <- prometheus.MustNewConstMetric(activeWorkflowsDesc, prometheus.GaugeValue, float64(snapshot.ActiveWorkflows))
	ch <- prometheus.MustNewConstMetric(scheduledJobsDesc, prometheus.GaugeValue, float64(snapshot.ScheduledJobs))
	ch <- prometheus.MustNewConstMetric(enabledRunnersDesc, prometheus.GaugeValue, float64(snapshot.EnabledRunners))
	if snapshot.HeartbeatKnown {
		ch <- prometheus.MustNewConstMetric(heartbeatAgeDesc, prometheus.GaugeValue, snapshot.OldestHeartbeatAge.Seconds())
	}
}

// Server exposes the orchestrator's registry over HTTP at /metrics.
type Server struct {
	bind     string
	recorder *Recorder
	server   *http.Server
	listener net.Listener
}

// New builds the metrics server for bind, reading state from source. An empty
// bind disables the listener: the recorder and the handler still exist, so
// nothing that reports into them has to know, but no port is opened.
func New(bind string, source Source) *Server { return NewWithClock(bind, source, time.Now) }

// NewWithClock is New with an injectable clock, so a test can watch an age
// series grow without waiting on wall time.
func NewWithClock(bind string, source Source, now func() time.Time) *Server {
	recorder := newRecorder(now)
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		recorder.loopRuns,
		&collector{source: source, recorder: recorder, timeout: scrapeTimeout},
	)
	mux := http.NewServeMux()
	// MaxRequestsInFlight bounds how many scrapes may hold a database
	// connection at once. Without it the endpoint is an unauthenticated way to
	// drain the connection pool: every concurrent GET runs a query that holds
	// one pooled connection for up to scrapeTimeout, and the pool is shared
	// with the scheduler tick, runner heartbeats and job events. Two, so a
	// second Prometheus (a rolling replacement, a one-off curl) is served
	// rather than refused, and the pool — max(4, NumCPU) — keeps headroom for
	// the control plane. Anything beyond that gets 503, which is the honest
	// answer to "scrape faster than I can read".
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: 2}))
	return &Server{
		bind:     bind,
		recorder: recorder,
		server:   &http.Server{Addr: bind, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
	}
}

// Enabled reports whether a listener was configured.
func (s *Server) Enabled() bool { return s != nil && s.bind != "" }

// Recorder returns the recorder this server exports.
func (s *Server) Recorder() *Recorder {
	if s == nil {
		return nil
	}
	return s.recorder
}

// Handler returns the metrics handler, for callers that serve it themselves.
func (s *Server) Handler() http.Handler {
	if s == nil {
		return http.NotFoundHandler()
	}
	return s.server.Handler
}

// Addr is the address actually bound, which is not the configured one when the
// configured port was 0. Empty until Start succeeds.
func (s *Server) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Start binds the listener and serves in the background. The bind happens here
// rather than inside the goroutine so a port that is taken, or an address that
// cannot be bound, is reported to the caller instead of disappearing into a
// discarded error — a metrics endpoint that silently never listens is the same
// class of defect as a gauge that silently never moves. Serving itself is
// backgrounded, so this never delays the control plane coming up.
func (s *Server) Start() error {
	if !s.Enabled() {
		slog.Info("metrics endpoint disabled", "reason", "LOOP_METRICS_BIND is empty")
		return nil
	}
	listener, err := net.Listen("tcp", s.bind)
	if err != nil {
		return fmt.Errorf("serve metrics on %q: %w", s.bind, err)
	}
	s.listener = listener
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server stopped", "error", err)
		}
	}()
	return nil
}

// Shutdown stops the listener, waiting for in-flight scrapes to finish or for
// ctx to expire. It is a no-op when the endpoint was never started.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}
