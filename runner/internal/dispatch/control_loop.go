package dispatch

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/metrics"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

const defaultEventBufferSize = 128

// A reduced terminal payload keeps its string fields within this budget so that
// an unbounded one — `error` carries whatever the agent wrote to stderr — cannot
// push the retry back over the event payload limit.
//
// The budget is the JSON-*encoded* size, for the same reason maxLogTailBytes is
// (see logtail.go): the 16 KiB event cap applies to the encoded payload, and
// Go's encoder spends six bytes on each `<`, `>`, `&`, and control byte. Three
// raw-measured 2 KiB fields of angle brackets encode to 36 KiB — over the cap
// the reduction exists to get under, which would lose the run's outcome
// entirely.
const maxTerminalPayloadFieldBytes = 2048

// `remainingWork` is written by the agent and is kept by the reduced payload,
// so — unlike the raw result document, which the reduction drops — it has to be
// bounded where it is built. Both bounds apply: the entry count keeps the
// encoded list short, the byte budget keeps a single verbose entry from
// spending the whole payload. Measured encoded, as above.
const maxTerminalPayloadListItems = 20
const maxTerminalPayloadListBytes = 2048

const truncationMarker = "… [truncated]"

type ControlClient interface {
	control.OfferClient
	control.EventClient
	// SetDraining reports whether the runner wants new work. It is part of the
	// interface rather than an optional capability discovered by type assertion
	// because a client that cannot report its drain state silently strands the
	// runner: `app.runners.draining` gates every placement query and nothing
	// else clears it (issue #148).
	SetDraining(bool) error
	Receive() (*runnerv1.OrchestratorToRunner, error)
	Disconnect()
}

type executionDispatcher interface {
	Execute(context.Context, control.Lease) (Result, error)
}

type cancellableDispatcher interface {
	Cancel(context.Context, control.Lease) error
}

type activeExecution struct {
	lease     control.Lease
	cancel    context.CancelFunc
	cancelled bool
	terminal  bool
}

type ControlLoop struct {
	Client         ControlClient
	Offers         *control.OfferState
	Reporter       *control.EventReporter
	Dispatcher     executionDispatcher
	Logger         *slog.Logger
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	ExpiryInterval time.Duration
	// OfferTimeout bounds how long an accepted offer may wait for its lease
	// acknowledgement before the reservation is released. Non-positive falls
	// back to control.DefaultOfferTimeout.
	OfferTimeout time.Duration

	// Metrics records the execution counters and the busy state this loop is
	// the only component that observes. Set it through UseMetrics so the event
	// reporter is pointed at the same recorder; nil falls back to the
	// process-wide recorder the metrics server exports.
	Metrics *metrics.Recorder

	mu       sync.Mutex
	draining bool
	active   map[string]*activeExecution

	// drainReports serializes each drain report with the read of the state it
	// reports, so no delivered report can be overtaken by a staler one. Without
	// it a Drain() concurrent with a reconnect could set the flag and send
	// `true`, only for the reconnect's already-read `false` to land afterwards
	// and advertise a draining runner as available. It orders delivery, not
	// success: a report lost on a dying stream is re-asserted by the next
	// Resume, which is the recovery this whole mechanism rests on.
	//
	// Deliberately not `mu`: a report blocks for as long as a gRPC send does,
	// and `mu` also guards the active-execution map that WaitForIdle and every
	// terminal execution touch. Nothing acquires `drainReports` while holding
	// `mu`, so the two never deadlock.
	drainReports sync.Mutex

	// busyReports serializes the read of the capacity state with the gauge
	// write that reports it, for the same reason drainReports exists. Without
	// it, a publisher that read "at capacity" can be overtaken by one that read
	// "idle" a moment later and land afterwards, leaving the gauge asserting a
	// busy runner that has been idle since. `Handle`'s publish races exactly
	// that way against a fast execution finishing on its own goroutine.
	// Nothing acquires busyReports while holding `mu` or the offer state's
	// lock, so it cannot deadlock either.
	busyReports sync.Mutex
}

func NewControlLoop(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration) (*ControlLoop, error) {
	return NewControlLoopWithRedaction(client, dispatcher, now, leaseDuration, renewalLead, nil)
}

func NewControlLoopWithRedaction(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string) (*ControlLoop, error) {
	return NewControlLoopWithOutbox(client, dispatcher, now, leaseDuration, renewalLead, redactionPrefixes, "")
}

func NewControlLoopWithOutbox(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string, outboxPath string) (*ControlLoop, error) {
	return NewControlLoopWithCapacity(client, dispatcher, now, leaseDuration, renewalLead, redactionPrefixes, outboxPath, 1)
}

// NewControlLoopWithCapacity allows the runner to work on up to `capacity`
// executions concurrently (e.g. for different projects). ProjectConcurrencyGuard
// still serializes executions that share a project's worktree.
func NewControlLoopWithCapacity(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string, outboxPath string, capacity int) (*ControlLoop, error) {
	return NewControlLoopWithEventBuffer(client, dispatcher, now, leaseDuration, renewalLead, redactionPrefixes, outboxPath, capacity, defaultEventBufferSize)
}

func NewControlLoopWithEventBuffer(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string, outboxPath string, capacity, eventBufferSize int) (*ControlLoop, error) {
	return NewControlLoopWithEventLimits(client, dispatcher, now, leaseDuration, renewalLead, redactionPrefixes, outboxPath, capacity, eventBufferSize, 16*1024, 6*1024)
}

func NewControlLoopWithEventLimits(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string, outboxPath string, capacity, eventBufferSize, eventPayloadBytes, logChunkBytes int) (*ControlLoop, error) {
	if client == nil || now == nil {
		return nil, errors.New("runner control loop dependencies are required")
	}
	if capacity < 1 {
		capacity = 1
	}
	offers, err := control.NewOfferStateWithCapacity(client, now, leaseDuration, renewalLead, capacity)
	if err != nil {
		return nil, err
	}
	if eventBufferSize < 1 {
		eventBufferSize = defaultEventBufferSize
	}
	// Each execution owes a terminal event, and an expired lease frees its
	// capacity slot (control.OfferState.Expire) while the superseded execution
	// is still winding down and still owes one. Two terminal slots per unit of
	// capacity covers that overlap. It is a floor, not a guarantee: a longer
	// pile-up of undelivered terminal events can still exhaust the buffer, in
	// which case the loss is reported by emitEvent rather than passing silently.
	if eventBufferSize < 2*capacity {
		eventBufferSize = 2 * capacity
	}
	reporter, err := control.NewEventReporterWithLimits(client, eventBufferSize, redactionPrefixes, outboxPath, eventPayloadBytes, logChunkBytes)
	if err != nil {
		return nil, err
	}
	return &ControlLoop{Client: client, Offers: offers, Reporter: reporter, Dispatcher: dispatcher, active: map[string]*activeExecution{}}, nil
}

// Run receives and handles control messages until ctx is cancelled or a
// message handler returns an error. Reconnection of the underlying
// transport is owned entirely by control.StreamSupervisor; Run never calls
// Client.Disconnect itself. While the client is disconnected, Receive
// returns control.ErrNotConnected immediately, so Run applies its own
// jittered exponential backoff (bounded by ReconnectMin/ReconnectMax)
// between retries instead of hot-looping.
func (loop *ControlLoop) Run(ctx context.Context) error {
	if err := loop.validate(); err != nil {
		return err
	}
	backoff := loop.reconnectMin()
	expiryTicker := time.NewTicker(loop.expiryInterval())
	defer expiryTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-expiryTicker.C:
				loop.expire()
			}
		}
	}()
	for {
		message, err := loop.Client.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			delay := jitterReconnectDelay(backoff, loop.reconnectMax())
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			backoff = nextReconnectBackoff(backoff, loop.reconnectMax())
			continue
		}
		backoff = loop.reconnectMin()
		if err := loop.Handle(ctx, message); err != nil {
			loop.logger().Warn("runner control message rejected", "error", err)
			continue
		}
	}
}

// UseMetrics directs this loop and its event reporter at one recorder. Call it
// before Run; a nil recorder restores the process-wide default.
func (loop *ControlLoop) UseMetrics(recorder *metrics.Recorder) {
	if loop == nil {
		return
	}
	loop.Metrics = recorder
	if loop.Reporter != nil {
		loop.Reporter.Metrics = recorder
		// The reporter published whatever it recovered from the outbox before
		// this recorder existed; republish so the new one starts from the truth
		// rather than from zero.
		loop.Reporter.PublishMetrics()
	}
	loop.publishBusy()
}

func (loop *ControlLoop) recorder() *metrics.Recorder {
	if loop != nil && loop.Metrics != nil {
		return loop.Metrics
	}
	return metrics.Default()
}

// publishBusy republishes the busy gauge from Busy() rather than tracking the
// transitions itself, so the gauge cannot drift out of step with the predicate
// OfferState.Admit actually uses. It is called wherever capacity can change:
// after handling a control message, after an expiry sweep, and once an
// execution has released its lease.
func (loop *ControlLoop) publishBusy() {
	if loop == nil || loop.Offers == nil {
		return
	}
	loop.busyReports.Lock()
	defer loop.busyReports.Unlock()
	loop.recorder().SetBusy(loop.Busy())
}

func (loop *ControlLoop) reconnectMin() time.Duration {
	if loop != nil && loop.ReconnectMin > 0 {
		return loop.ReconnectMin
	}
	return time.Second
}

func (loop *ControlLoop) reconnectMax() time.Duration {
	minimum := loop.reconnectMin()
	if loop != nil && loop.ReconnectMax >= minimum {
		return loop.ReconnectMax
	}
	return 60 * time.Second
}

func (loop *ControlLoop) Handle(ctx context.Context, message *runnerv1.OrchestratorToRunner) error {
	if err := loop.validate(); err != nil {
		return err
	}
	// Admissions, acknowledgements and abandonments all change capacity, and
	// each has several exits; publishing once on the way out covers them all.
	defer loop.publishBusy()
	if message == nil {
		return errors.New("orchestrator control message is required")
	}
	if offer := message.GetOffer(); offer != nil {
		loop.logger().Info("runner received job offer", "job_id", offer.GetJobId(), "lease_generation", offer.GetLeaseGeneration())
		if loop.Draining() {
			if err := loop.Client.RejectOffer(offer.GetJobId(), "runner is draining"); err != nil {
				loop.logger().Warn("could not reject offer while draining", "job_id", offer.GetJobId(), "error", err)
			}
			return nil
		}
		if _, err := loop.Offers.Admit(offer); err != nil {
			loop.logger().Warn("could not process job offer", "job_id", offer.GetJobId(), "error", err)
		}
		return nil
	}
	if message.GetDrain() != nil {
		loop.Drain()
		return nil
	}
	if cancellation := message.GetCancel(); cancellation != nil {
		loop.logger().Info("runner received execution cancellation", "execution_id", cancellation.GetExecutionId(), "lease_generation", cancellation.GetLeaseGeneration())
		loop.Cancel(cancellation.GetExecutionId(), cancellation.GetLeaseGeneration())
		return nil
	}
	if acknowledgement := message.GetLeaseAcknowledged(); acknowledgement != nil {
		jobID := acknowledgement.GetJobId()
		held, alreadyActive := loop.Offers.ActiveLease(jobID)
		if !loop.Offers.ApplyAcknowledgement(acknowledgement) {
			// A duplicate or non-advancing renewal acknowledgement for a lease
			// the runner still holds at the same generation is routine and
			// changes nothing. Anything else matched neither a reservation nor
			// a lease — most often a reservation that timed out waiting for
			// exactly this message — and dropping it silently made a released
			// capacity slot indistinguishable from a lost one.
			if !alreadyActive || held.Generation != acknowledgement.GetLeaseGeneration() {
				loop.logger().Warn("runner discarded an unmatched lease acknowledgement", "job_id", jobID, "lease_generation", acknowledgement.GetLeaseGeneration())
			}
			return nil
		}
		if alreadyActive {
			return nil
		}
		lease, ok := loop.Offers.ActiveLease(jobID)
		if !ok {
			loop.logger().Warn("acknowledged lease is unavailable", "job_id", jobID)
			return nil
		}
		if err := loop.Reporter.Begin(lease); err != nil {
			loop.Offers.Abandon(lease.JobID, lease.Generation)
			if errors.Is(err, control.ErrStaleEventLease) {
				loop.logger().Warn("stale execution acknowledgement", "job_id", jobID, "error", err)
				return nil
			}
			return fmt.Errorf("begin execution event reporting: %w", err)
		}
		executionContext, cancel := context.WithCancel(context.Background())
		loop.mu.Lock()
		loop.active[lease.JobID] = &activeExecution{lease: lease, cancel: cancel}
		loop.mu.Unlock()
		loop.logger().Info("runner execution acknowledged", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation)
		go loop.execute(executionContext, lease)
		return nil
	}
	loop.logger().Warn("runner received unsupported control message, skipping", "message_type", fmt.Sprintf("%T", message.GetMessage()))
	return nil
}

func (loop *ControlLoop) Drain() {
	if loop == nil {
		return
	}
	loop.mu.Lock()
	alreadyDraining := loop.draining
	loop.draining = true
	loop.mu.Unlock()
	if alreadyDraining {
		return
	}
	// A failed report is not fatal here: the drain still holds locally, so no
	// new offer is accepted, and the next stream Resume re-asserts it. That
	// recovery is the whole point of reporting the state on connect — before
	// it existed, a report lost on a dying stream was lost for good.
	if err := loop.reportDrainState(); err != nil {
		loop.logger().Warn("report runner draining", "error", err)
	}
}

// Resume re-establishes this runner's state on a freshly connected control
// stream: it reports the runner's drain state, then delivers any events
// buffered while the stream was down.
//
// The drain report is what keeps the orchestrator's view of the runner honest
// across a restart. `app.runners.draining` gates every placement query and the
// runner is its only writer today, so a runner that reported `true`, exited,
// and came back with the same identity would otherwise reconnect into a
// permanent `true` and never be offered work again (issue #148).
//
// It reports Draining(), never a bare `false`: a runner that reconnects while
// genuinely draining — a network blip mid-drain, or an orchestrator-initiated
// drain whose report never left the old stream — re-asserts the drain instead
// of advertising itself as available.
//
// The report precedes the flush so the orchestrator stops placing work before
// the runner spends the fresh stream on a backlog of events. A failed report
// fails the resume — continuing would leave the orchestrator with a stale view
// for the whole life of the connection — which makes StreamSupervisor drop the
// stream and retry when the failure is transient, and stop the runner when it
// is not. That is the same treatment the buffered-event flush already got.
func (loop *ControlLoop) Resume() error {
	if err := loop.validate(); err != nil {
		return err
	}
	if err := loop.reportDrainState(); err != nil {
		return fmt.Errorf("report runner drain state: %w", err)
	}
	return loop.FlushEvents()
}

// reportDrainState sends the runner's current drain state. The state is read
// while drainReports is held so a concurrent reporter cannot send a value that
// is already stale by the time it reaches the wire; see the field's comment.
func (loop *ControlLoop) reportDrainState() error {
	// Resume validates first, and the constructors reject a nil client, so this
	// only covers a ControlLoop assembled field by field calling Drain() —
	// which guards its own receiver and nothing else. An error beats a panic.
	if loop == nil || loop.Client == nil {
		return errors.New("runner control client is required")
	}
	loop.drainReports.Lock()
	defer loop.drainReports.Unlock()
	return loop.Client.SetDraining(loop.Draining())
}

func (loop *ControlLoop) Draining() bool {
	if loop == nil {
		return false
	}
	loop.mu.Lock()
	defer loop.mu.Unlock()
	return loop.draining
}

func (loop *ControlLoop) Cancel(executionID string, generation int64) bool {
	if loop == nil || executionID == "" || generation < 1 {
		return false
	}
	loop.mu.Lock()
	var active *activeExecution
	for _, candidate := range loop.active {
		if !candidate.terminal && candidate.lease.Packet.ExecutionID == executionID && candidate.lease.Generation == generation {
			active = candidate
			break
		}
	}
	if active == nil {
		loop.mu.Unlock()
		return false
	}
	active.cancelled = true
	active.cancel()
	lease := active.lease
	loop.mu.Unlock()
	if dispatcher, ok := loop.Dispatcher.(cancellableDispatcher); ok {
		go func() { _ = dispatcher.Cancel(context.Background(), lease) }()
	}
	return true
}

func (loop *ControlLoop) CancelAll() {
	if loop == nil {
		return
	}
	loop.mu.Lock()
	leases := make([]control.Lease, 0, len(loop.active))
	for _, active := range loop.active {
		if active.terminal || active.cancelled {
			continue
		}
		active.cancelled = true
		active.cancel()
		leases = append(leases, active.lease)
	}
	loop.mu.Unlock()
	if dispatcher, ok := loop.Dispatcher.(cancellableDispatcher); ok {
		for _, lease := range leases {
			go func(lease control.Lease) { _ = dispatcher.Cancel(context.Background(), lease) }(lease)
		}
	}
}

func (loop *ControlLoop) ReportRecoveredFailure(jobID string, generation int64, executionID string) error {
	if loop == nil || loop.Reporter == nil || jobID == "" || generation < 1 || executionID == "" {
		return errors.New("recovered execution is invalid")
	}
	sequence, err := loop.Reporter.RecoverTerminal(control.Lease{JobID: jobID, Generation: generation, Packet: taskpacket.Packet{JobID: jobID, ExecutionID: executionID}}, "failed", map[string]any{
		"status": "failed",
		"error":  "runner restarted while this execution was active",
	})
	if sequence > 0 {
		return nil
	}
	return err
}

func (loop *ControlLoop) Reconcile() error {
	if err := loop.validate(); err != nil {
		return err
	}
	loop.expire()
	if _, err := loop.Offers.RenewDue(); err != nil {
		return fmt.Errorf("renew active lease: %w", err)
	}
	loop.publishBusy()
	return nil
}

func (loop *ControlLoop) expiryInterval() time.Duration {
	if loop != nil && loop.ExpiryInterval > 0 {
		return loop.ExpiryInterval
	}
	return time.Second
}

func (loop *ControlLoop) offerTimeout() time.Duration {
	if loop != nil && loop.OfferTimeout > 0 {
		return loop.OfferTimeout
	}
	return control.DefaultOfferTimeout
}

func (loop *ControlLoop) expire() {
	if loop == nil || loop.Offers == nil || loop.Reporter == nil {
		return
	}
	defer loop.publishBusy()
	// A reservation has no execution and no event lease — Reporter.Begin runs
	// only once the acknowledgement lands — so releasing the slot and recording
	// the offer is the whole of the work here.
	timeout := loop.offerTimeout()
	for _, reservation := range loop.Offers.ExpirePending(timeout) {
		loop.logger().Warn("runner offer reservation expired without a lease acknowledgement",
			"job_id", reservation.JobID, "execution_id", reservation.ExecutionID,
			"lease_generation", reservation.Generation, "reserved_at", reservation.ReservedAt, "offer_timeout", timeout)
	}
	for _, expired := range loop.Offers.Expire() {
		loop.logger().Warn("runner lease expired", "job_id", expired.JobID, "execution_id", expired.Packet.ExecutionID, "lease_generation", expired.Generation)
		loop.cancelExpired(expired)
		loop.Reporter.Abandon(expired.JobID, expired.Generation)
	}
}

func (loop *ControlLoop) FlushEvents() error {
	if err := loop.validate(); err != nil {
		return err
	}
	if loop.Reporter.Pending() == 0 {
		return nil
	}
	return loop.Reporter.Flush()
}

func (loop *ControlLoop) logger() *slog.Logger {
	if loop != nil && loop.Logger != nil {
		return loop.Logger
	}
	return slog.Default()
}

// WaitForIdle blocks until no execution is running or ctx is done.
func (loop *ControlLoop) WaitForIdle(ctx context.Context) error {
	if loop == nil {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		loop.mu.Lock()
		idle := len(loop.active) == 0
		loop.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Busy reports whether the runner is at its concurrency capacity. It counts
// reservations as well as acknowledged leases so the heartbeat matches the
// predicate OfferState.Admit uses: a runner that would reject the next offer as
// busy must never advertise itself as available.
func (loop *ControlLoop) Busy() bool {
	if loop == nil || loop.Offers == nil {
		return false
	}
	return loop.Offers.ReservedCount() >= loop.Offers.Capacity()
}

func (loop *ControlLoop) execute(ctx context.Context, lease control.Lease) {
	// Deferred so a panic while building the terminal payload cannot leak the
	// reporter's retained lease record.
	defer func() {
		if !loop.Reporter.Finish(lease.JobID, lease.Generation) {
			loop.logger().Warn("execution finished without a matching event lease", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation)
		}
	}()
	started := time.Now()
	loop.logger().Info("runner execution started", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation)
	loop.recorder().RecordExecutionStarted()
	loop.emitEvent(lease, "started", map[string]any{"status": "running"})
	result, err := loop.Dispatcher.Execute(ctx, lease)
	usage := executionUsage(started, result)
	cancelled := loop.finish(lease) || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	if cancelled {
		payload := terminalPayload("cancelled", result, usage)
		loop.emitEvent(lease, "cancelled", payload)
	} else if err != nil || result.Status != "completed" {
		// An agent that ended cleanly and wrote `blocked` in its result
		// document stopped deliberately and said why. Reporting that as an
		// anonymous failure throws away the most actionable signal a run can
		// produce, so the payload keeps the distinction. The event *type* stays
		// `failed`: it is a shared contract with the orchestrator
		// (VALID_EVENT_TYPES), the transport, and the job status column, and a
		// block is a refinement of a non-delivering outcome rather than a new
		// kind of one. A failing process is never trusted to report a block —
		// its own account of why it stopped is not evidence.
		blocked := err == nil && result.Status == "blocked"
		status := "failed"
		if blocked {
			status = "blocked"
		}
		failure := err
		if failure == nil {
			failure = errors.New(agentFailureText(result))
		}
		payload := terminalPayload(status, result, usage)
		if blocked {
			payload["blocked"] = true
		}
		payload["failureFingerprint"] = FailureFingerprint("execution", failure)
		payload["error"] = failure.Error()
		loop.emitEvent(lease, "failed", payload)
	} else {
		payload := terminalPayload(result.Status, result, usage)
		loop.emitEvent(lease, "completed", payload)
	}
	status := terminalStatus(cancelled, err, result)
	loop.logger().Info("runner execution terminal", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation, "status", status)
	loop.recorder().RecordExecutionCompleted(status)
	loop.Offers.Abandon(lease.JobID, lease.Generation)
	loop.publishBusy()
}

// emitEvent reports an execution lifecycle event and, unlike a discarded
// error, makes both failure modes visible. A non-zero sequence means the event
// was queued (and persisted to the outbox when one is configured) and delivery
// is retried on reconnect; a zero sequence means the event was never queued,
// which for a terminal event is a lost run outcome and is logged at error
// level. A terminal event that could not be queued is retried once stripped to
// its essential fields, since an oversized payload is the failure mode a
// successful large run is most likely to hit.
func (loop *ControlLoop) emitEvent(lease control.Lease, eventType string, payload map[string]any) {
	sequence, err := loop.Reporter.Emit(lease.JobID, lease.Generation, eventType, payload)
	if err == nil {
		return
	}
	if sequence == 0 && control.IsTerminalEventType(eventType) {
		if reduced := minimalTerminalPayload(payload); reduced != nil {
			if retrySequence, retryErr := loop.Reporter.Emit(lease.JobID, lease.Generation, eventType, reduced); retrySequence > 0 {
				loop.logger().Warn("terminal execution event reduced to its essential fields after a failed emit",
					"job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation,
					"event_type", eventType, "event_sequence", retrySequence, "error", err)
				sequence, err = retrySequence, retryErr
				if err == nil {
					return
				}
			}
		}
	}
	fields := []any{
		"job_id", lease.JobID,
		"execution_id", lease.Packet.ExecutionID,
		"lease_generation", lease.Generation,
		"event_type", eventType,
		"error", err,
	}
	if sequence > 0 {
		// Queued but not yet delivered: the outbox will retry it, so this is
		// not a loss and must not be counted as one.
		loop.logger().Warn("execution event queued for delivery after a transport failure", append(fields, "event_sequence", sequence)...)
		return
	}
	// Nothing was queued and the retry above — where there was one — did not
	// save it either, so the event is gone. This is the only place a rejected
	// lifecycle event is counted: the reporter deliberately does not count its
	// own rejections, because the reduced-payload retry above turns the most
	// common one into a delivered event, and counting at rejection would book a
	// drop against a run that reported its outcome perfectly well.
	loop.recorder().RecordEventDropped(control.DropReason(err))
	if control.IsTerminalEventType(eventType) {
		loop.logger().Error("terminal execution event lost", fields...)
		return
	}
	loop.logger().Warn("execution event dropped", fields...)
}

// minimalTerminalPayload strips a terminal payload down to the fields the
// orchestrator needs to classify the outcome, dropping the unbounded ones
// (changed files, commands, the raw result document) and truncating the fields
// it keeps. Keeping `error` is not enough on its own: it carries whatever the
// agent wrote to stderr, so a wedged agent can blow the payload limit with that
// field alone. It returns nil when nothing was dropped or truncated, so the
// caller does not re-emit an identical event that would fail the same way.
func minimalTerminalPayload(payload map[string]any) map[string]any {
	reduced := make(map[string]any, len(minimalTerminalPayloadKeys))
	truncated := false
	for _, key := range minimalTerminalPayloadKeys {
		value, present := payload[key]
		if !present {
			continue
		}
		// Every kept field is re-bounded here rather than trusted: `error` is
		// built by the caller and never passed through terminalPayload, so this
		// is the only place it is measured at all.
		switch typed := value.(type) {
		case string:
			if shortened := boundedAgentText(typed, maxTerminalPayloadFieldBytes); shortened != typed {
				value = shortened
				truncated = true
			}
		case []string:
			if shortened := boundedList(typed); !equalStrings(shortened, typed) {
				value = shortened
				truncated = true
			}
		}
		reduced[key] = value
	}
	for key := range payload {
		if _, kept := reduced[key]; !kept {
			return reduced
		}
	}
	if truncated {
		return reduced
	}
	return nil
}

// The work-in-progress fields are kept because they are short and are the only
// pointer to a failed run's surviving work; the log tail is not, since it is the
// kind of unbounded detail the reduced payload exists to shed. The block fields
// are kept for the same reason as the work-in-progress ones: the reduction runs
// exactly when a verbose agent produced an oversized payload, which is when
// dropping its explanation would leave the orchestrator with nothing but a
// fingerprint. They are safe to keep because terminalPayload bounds them; the
// raw result document is dropped because nothing bounds it.
var minimalTerminalPayloadKeys = []string{"status", "exitCode", "error", "failureFingerprint", "durationMs", "branch", "wipBranch", "wipCommit", "wipPushed", "blocked", "summary", "remainingWork", "continuations", "gateVerdict"}

// boundedAgentText renders agent-written text as a payload-safe string: it
// drops terminal escape sequences and other control characters, replaces
// invalid UTF-8, and keeps the longest prefix whose JSON encoding fits budget,
// marking it when anything was cut. It is boundedLogTail's mirror image —
// a log tail is interesting at its end, an agent's prose at its beginning.
func boundedAgentText(value string, budget int) string {
	sanitized := strings.TrimSpace(strings.ToValidUTF8(sanitizeLogText(value), ""))
	if jsonEncodedSize(sanitized) <= budget {
		return sanitized
	}
	remaining := budget - jsonEncodedSize(truncationMarker)
	end := 0
	for end < len(sanitized) {
		next := end + 1
		for next < len(sanitized) && !utf8.RuneStart(sanitized[next]) {
			next++
		}
		cost := jsonEncodedSize(sanitized[end:next])
		if remaining < cost {
			break
		}
		remaining -= cost
		end = next
	}
	return sanitized[:end] + truncationMarker
}

// boundedList caps an agent-written list of strings by entry count and by the
// total JSON-encoded size of the agent text it may carry, so neither a long
// list nor one verbose entry can spend the whole event payload. What was cut is
// stated in a trailing entry, so a reader can tell a complete list from a
// truncated one. Blank entries are dropped rather than forwarded as empty
// strings. The result encodes to at most maxTerminalPayloadListBytes bytes plus
// one trailing marker.
func boundedList(values []string) []string {
	bounded := make([]string, 0, min(len(values), maxTerminalPayloadListItems)+1)
	budget := maxTerminalPayloadListBytes
	for index, value := range values {
		if len(bounded) >= maxTerminalPayloadListItems || budget <= 0 {
			return append(bounded, fmt.Sprintf("%s %d more", truncationMarker, len(values)-index))
		}
		// Bounded against what is left rather than dropped: one long entry is
		// often the whole of what the agent had to say about the work ahead.
		entry := boundedAgentText(value, budget)
		if entry == "" {
			continue
		}
		budget -= jsonEncodedSize(entry)
		bounded = append(bounded, entry)
	}
	if len(bounded) == 0 {
		return nil
	}
	return bounded
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func executionUsage(started time.Time, result Result) map[string]any {
	return map[string]any{
		"durationMs":           time.Since(started).Milliseconds(),
		"changedFileCount":     len(result.ChangedFiles),
		"commandCount":         len(result.CommandsRun),
		"pipelineCommandCount": len(result.PipelineResults),
	}
}

func terminalStatus(cancelled bool, err error, result Result) string {
	if cancelled {
		return "cancelled"
	}
	if err == nil && result.Status == "blocked" {
		return "blocked"
	}
	if err != nil || result.Status != "completed" {
		return "failed"
	}
	return "completed"
}

// agentFailureText names a non-delivering outcome the executor did not itself
// fail on. The agent's summary is the reason when it wrote one; without it the
// status alone is reported, since an empty error string would yield an
// uninformative event and a fingerprint shared by every empty failure.
func agentFailureText(result Result) string {
	if result.Summary != "" {
		return result.Summary
	}
	if result.Status != "" {
		return "agent reported status " + result.Status + " without a summary"
	}
	return "agent reported no result status"
}

func terminalPayload(status string, result Result, usage map[string]any) map[string]any {
	payload := map[string]any{
		"status":        status,
		"exitCode":      result.ExitCode,
		"changedFiles":  result.ChangedFiles,
		"commandsRun":   result.CommandsRun,
		"finalRevision": result.FinalRevision,
		"committed":     result.Committed,
		"pushed":        result.Pushed,
	}
	if result.Branch != "" {
		payload["branch"] = result.Branch
	}
	// A non-delivering run reports where its work survived instead. `wipBranch`
	// is deliberately distinct from `branch`: the orchestrator can build a retry
	// packet from it without any consumer mistaking it for delivered work.
	if result.WorkInProgressCommit != "" {
		payload["wipCommit"] = result.WorkInProgressCommit
		payload["wipPushed"] = result.WorkInProgressPushed
	}
	if result.WorkInProgressBranch != "" {
		payload["wipBranch"] = result.WorkInProgressBranch
	}
	if result.LogTail != "" {
		payload["logTail"] = result.LogTail
	}
	// The goal gate's account of the run. Both fields are runner-derived and
	// carry no agent prose, so — unlike the agent's own summary below — they
	// are reported for every outcome including a cancelled one, and they cannot
	// destabilise a failure fingerprint. Without them a run delivered after two
	// continuations and one that gave up after three are indistinguishable:
	// they share a terminal status and differ only here.
	if result.Continuations > 0 {
		payload["continuations"] = result.Continuations
	}
	if result.GateVerdict != "" {
		payload["gateVerdict"] = result.GateVerdict
	}
	// The agent's own account of the run travels with every outcome the agent
	// itself reached. Withholding it from anything but a success is what made an
	// agent-reported block indistinguishable from a crash (issue #97): the
	// reason it stopped and the work it says remains are precisely what an
	// informed retry or escalation is built from.
	//
	// A cancelled run reached no outcome of its own — it was interrupted — so it
	// keeps reporting none. That is also what keeps repeated cancellations
	// identifiable: the orchestrator fingerprints them from the stable
	// `cancelled exit=N` text it derives when no `error` or `summary` is
	// present, and an interrupted agent's summary varies run to run.
	if status != "cancelled" {
		if result.Raw != nil {
			payload["result"] = result.Raw
		}
		if summary := boundedAgentText(result.Summary, maxTerminalPayloadFieldBytes); summary != "" {
			payload["summary"] = summary
		}
		if remaining := boundedList(result.RemainingWork); len(remaining) > 0 {
			payload["remainingWork"] = remaining
		}
	}
	for key, value := range usage {
		payload[key] = value
	}
	return payload
}

func (loop *ControlLoop) finish(lease control.Lease) bool {
	loop.mu.Lock()
	defer loop.mu.Unlock()
	active, ok := loop.active[lease.JobID]
	if !ok || active.lease.Generation != lease.Generation {
		return false
	}
	active.terminal = true
	cancelled := active.cancelled
	delete(loop.active, lease.JobID)
	return cancelled
}

func (loop *ControlLoop) cancelExpired(lease control.Lease) {
	loop.mu.Lock()
	active, ok := loop.active[lease.JobID]
	if !ok || active.lease.Generation != lease.Generation {
		loop.mu.Unlock()
		return
	}
	active.cancelled = true
	active.terminal = true
	active.cancel()
	delete(loop.active, lease.JobID)
	loop.mu.Unlock()
	if dispatcher, ok := loop.Dispatcher.(cancellableDispatcher); ok {
		go func() { _ = dispatcher.Cancel(context.Background(), lease) }()
	}
}

func (loop *ControlLoop) validate() error {
	if loop == nil || loop.Client == nil || loop.Offers == nil || loop.Reporter == nil || loop.Dispatcher == nil {
		return errors.New("runner control loop dependencies are required")
	}
	return nil
}

func nextReconnectBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func jitterReconnectDelay(base, maximum time.Duration) time.Duration {
	if base <= 1 {
		return base
	}
	delta, err := rand.Int(rand.Reader, big.NewInt(int64(base/4)+1))
	if err != nil {
		return base
	}
	delay := base + time.Duration(delta.Int64())
	if delay > maximum {
		return maximum
	}
	return delay
}
