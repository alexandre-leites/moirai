// Package metrics publishes the Prometheus metrics a runner owns.
//
// Every series here is written from state the runner itself holds. Values that
// belong to the orchestrator — eligible queue depth, active workflow counts,
// the fleet-wide runner heartbeat age — are deliberately absent: the runner
// cannot populate them, and exporting a constant zero for them is worse than
// exporting nothing, because an alert on a series that never changes can never
// fire (issue #124). The orchestrator exports the real ones from the database
// it owns (`moirai/observability.py`).
package metrics

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Execution outcomes reported by RecordExecutionCompleted. They mirror the
// terminal statuses the dispatch loop derives, and are enumerated so the label
// set stays bounded: a value outside this list is folded into OutcomeUnknown
// rather than minting a new time series.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeBlocked   = "blocked"
	OutcomeCancelled = "cancelled"
	OutcomeUnknown   = "unknown"
)

// Reasons reported by RecordEventDropped, bounded for the same reason as the
// execution outcomes above.
const (
	// DropBufferFull: the pending queue was full and nothing outranked by the
	// new event could be evicted to make room for it.
	DropBufferFull = "buffer_full"
	// DropEvicted: a queued event was discarded so a higher-priority one — in
	// practice a terminal event — could take its slot.
	DropEvicted = "evicted"
	// DropLeaseExpired: a job's queued log and progress events were discarded
	// when its lease expired.
	DropLeaseExpired = "lease_expired"
	// DropInvalid: the event type or its payload was rejected before queueing.
	DropInvalid = "invalid"
	// DropNoLease: no lease of the reported generation was held for the job.
	DropNoLease = "no_lease"
	// DropPersistFailed: the event could not be mirrored to the crash-safe
	// outbox, so it was rolled back out of the queue.
	DropPersistFailed = "persist_failed"
	DropUnknown       = "unknown"
)

var executionOutcomes = []string{OutcomeCompleted, OutcomeFailed, OutcomeBlocked, OutcomeCancelled, OutcomeUnknown}

var eventDropReasons = []string{DropBufferFull, DropEvicted, DropLeaseExpired, DropInvalid, DropNoLease, DropPersistFailed, DropUnknown}

// Recorder holds the runner-owned collectors and the registry they are
// exported from. Its methods are safe for concurrent use: every label child is
// resolved once at construction, so recording a value is an atomic add or store
// with no map lookup and no lock on the caller's path.
type Recorder struct {
	registry *prometheus.Registry
	now      func() time.Time

	// lastHeartbeat is the clock reading at the last successful heartbeat. It
	// starts at the recorder's construction time so a runner that never
	// completes a heartbeat still ages from process start, which is precisely
	// the condition an age alert has to catch.
	//
	// It stores the time.Time rather than Unix nanoseconds so the monotonic
	// reading time.Now() carries survives, and the subtraction in
	// HeartbeatAgeSeconds is monotonic-clock arithmetic. Stripping it to an
	// integer would make an NTP correction or a suspend/resume that steps the
	// wall clock backwards report an age of zero — "the heartbeat just
	// happened" — for the length of the step, which is the same lie issue #124
	// exists to remove.
	lastHeartbeat atomic.Pointer[time.Time]

	executionsStarted   prometheus.Counter
	executionsCompleted map[string]prometheus.Counter
	busy                prometheus.Gauge
	pendingEvents       prometheus.Gauge
	eventsDropped       map[string]prometheus.Counter
}

// defaultRecorder is the process-wide recorder. The runner assembles its
// components in several packages and none of them owns the metrics server, so
// each defaults to this recorder and New serves the same registry. Tests inject
// their own recorder instead.
var defaultRecorder = NewRecorder(time.Now)

// Default returns the process-wide recorder that New exports.
func Default() *Recorder { return defaultRecorder }

// NewRecorder builds an independent recorder reading time from now. A nil clock
// falls back to time.Now; tests pass a clock they can advance so the
// heartbeat-age gauge can be observed changing without waiting on wall time.
func NewRecorder(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	recorder := &Recorder{registry: prometheus.NewRegistry(), now: now}
	started := now()
	recorder.lastHeartbeat.Store(&started)

	recorder.executionsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "moirai_runner_executions_started_total",
		Help: "Executions this runner has started.",
	})
	completed := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "moirai_runner_executions_completed_total",
		Help: "Executions this runner has finished, by terminal outcome.",
	}, []string{"outcome"})
	recorder.busy = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "moirai_runner_busy",
		Help: "1 when this runner is at its concurrency capacity and would reject the next offer, 0 when it can accept work.",
	})
	recorder.pendingEvents = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "moirai_runner_pending_events",
		Help: "Execution events queued for delivery to the orchestrator.",
	})
	dropped := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "moirai_runner_events_dropped_total",
		Help: "Execution events this runner discarded rather than delivered, by reason.",
	}, []string{"reason"})

	// Every known label child is materialised now so each series exists at zero
	// from the first scrape. A counter that only appears once it is non-zero
	// cannot be alerted on with rate() until the first failure has happened.
	recorder.executionsCompleted = make(map[string]prometheus.Counter, len(executionOutcomes))
	for _, outcome := range executionOutcomes {
		recorder.executionsCompleted[outcome] = completed.WithLabelValues(outcome)
	}
	recorder.eventsDropped = make(map[string]prometheus.Counter, len(eventDropReasons))
	for _, reason := range eventDropReasons {
		recorder.eventsDropped[reason] = dropped.WithLabelValues(reason)
	}

	recorder.registry.MustRegister(
		recorder.executionsStarted,
		completed,
		recorder.busy,
		recorder.pendingEvents,
		dropped,
		// A GaugeFunc rather than a stored value: an age is only correct at the
		// moment it is read, so it is computed at scrape time from the injected
		// clock. The previous gauge was set to zero once at construction and
		// never written again, so it reported "the heartbeat just happened"
		// forever, including while the runner was disconnected.
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "moirai_runner_heartbeat_age_seconds",
			Help: "Seconds since this runner last completed a control-stream heartbeat. Counts from process start until the first heartbeat.",
		}, recorder.HeartbeatAgeSeconds),
	)
	return recorder
}

// Registry returns the registry these metrics are exported from.
func (r *Recorder) Registry() *prometheus.Registry { return r.registry }

// MarkHeartbeat records that a control-stream heartbeat has just succeeded.
func (r *Recorder) MarkHeartbeat() {
	if r == nil {
		return
	}
	at := r.now()
	r.lastHeartbeat.Store(&at)
}

// HeartbeatAgeSeconds reports the seconds elapsed since the last successful
// heartbeat. Both readings come from the same clock, so with the process clock
// the subtraction is monotonic and a wall-clock step cannot affect it. The
// negative clamp covers a clock that genuinely runs backwards — a test clock,
// or a monotonic-less one — since no consumer of an age has a meaning for a
// negative one.
func (r *Recorder) HeartbeatAgeSeconds() float64 {
	if r == nil {
		return 0
	}
	last := r.lastHeartbeat.Load()
	if last == nil {
		return 0
	}
	age := r.now().Sub(*last)
	if age < 0 {
		return 0
	}
	return age.Seconds()
}

// SetBusy publishes whether the runner is at its concurrency capacity.
func (r *Recorder) SetBusy(busy bool) {
	if r == nil {
		return
	}
	value := float64(0)
	if busy {
		value = 1
	}
	r.busy.Set(value)
}

// SetPendingEvents publishes the depth of the execution-event queue.
func (r *Recorder) SetPendingEvents(pending int) {
	if r == nil {
		return
	}
	r.pendingEvents.Set(float64(pending))
}

// RecordExecutionStarted counts an execution this runner has begun.
func (r *Recorder) RecordExecutionStarted() {
	if r == nil {
		return
	}
	r.executionsStarted.Inc()
}

// RecordExecutionCompleted counts an execution that reached a terminal
// outcome. An outcome outside the known set is counted as OutcomeUnknown so a
// new status string cannot silently grow the runner's label cardinality.
func (r *Recorder) RecordExecutionCompleted(outcome string) {
	if r == nil {
		return
	}
	counter, known := r.executionsCompleted[outcome]
	if !known {
		counter = r.executionsCompleted[OutcomeUnknown]
	}
	counter.Inc()
}

// RecordEventDropped counts one discarded execution event.
func (r *Recorder) RecordEventDropped(reason string) {
	r.RecordEventsDropped(reason, 1)
}

// RecordEventsDropped counts count discarded execution events. An unknown
// reason is counted as DropUnknown, bounding the label set the same way
// RecordExecutionCompleted does.
func (r *Recorder) RecordEventsDropped(reason string, count int) {
	if r == nil || count <= 0 {
		return
	}
	counter, known := r.eventsDropped[reason]
	if !known {
		counter = r.eventsDropped[DropUnknown]
	}
	counter.Add(float64(count))
}

// Server exposes a recorder's registry over HTTP at /metrics.
type Server struct {
	server   *http.Server
	recorder *Recorder
}

// New serves the process-wide recorder on bind.
func New(bind string) *Server { return NewWithRecorder(bind, Default()) }

// NewWithRecorder serves a specific recorder, so a test can exercise the same
// handler the runner exposes without touching process-wide state.
func NewWithRecorder(bind string, recorder *Recorder) *Server {
	if recorder == nil {
		recorder = Default()
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(recorder.Registry(), promhttp.HandlerOpts{}))
	return &Server{
		server:   &http.Server{Addr: bind, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		recorder: recorder,
	}
}

func (s *Server) Start() {
	go func() { _ = s.server.ListenAndServe() }()
}

// Handler returns the metrics handler, for callers that serve it themselves.
func (s *Server) Handler() http.Handler { return s.server.Handler }

// Recorder returns the recorder this server exports.
func (s *Server) Recorder() *Recorder { return s.recorder }

// MarkHeartbeat records a successful control-stream heartbeat.
func (s *Server) MarkHeartbeat() { s.recorder.MarkHeartbeat() }
