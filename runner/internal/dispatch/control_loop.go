package dispatch

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"
	"unicode/utf8"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
)

const defaultEventBufferSize = 128

// A reduced terminal payload keeps its string fields within this budget so that
// an unbounded one — `error` carries whatever the agent wrote to stderr — cannot
// push the retry back over the event payload limit.
const maxTerminalPayloadFieldBytes = 2048

const truncationMarker = "… [truncated]"

type ControlClient interface {
	control.OfferClient
	control.EventClient
	Receive() (*runnerv1.OrchestratorToRunner, error)
	Disconnect()
}

type executionDispatcher interface {
	Execute(context.Context, control.Lease) (Result, error)
}

type cancellableDispatcher interface {
	Cancel(context.Context, control.Lease) error
}

type drainingClient interface {
	SetDraining(bool) error
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

	mu       sync.Mutex
	draining bool
	active   map[string]*activeExecution
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
	reporter, err := control.NewEventReporter(client, eventBufferSize, redactionPrefixes, outboxPath)
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
			if errors.Is(err, control.ErrNotConnected) || errors.Is(err, control.ErrStaleEventLease) {
				loop.logger().Warn("recoverable runner control race", "error", err)
				continue
			}
			return err
		}
	}
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
	if !alreadyDraining {
		if client, ok := loop.Client.(drainingClient); ok {
			if err := client.SetDraining(true); err != nil {
				loop.logger().Warn("report runner draining", "error", err)
			}
		}
	}
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

func (loop *ControlLoop) Reconcile() error {
	if err := loop.validate(); err != nil {
		return err
	}
	loop.expire()
	if _, err := loop.Offers.RenewDue(); err != nil {
		return fmt.Errorf("renew active lease: %w", err)
	}
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
	loop.emitEvent(lease, "started", map[string]any{"status": "running"})
	result, err := loop.Dispatcher.Execute(ctx, lease)
	usage := executionUsage(started, result)
	cancelled := loop.finish(lease) || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	if cancelled {
		payload := terminalPayload("cancelled", result, usage)
		loop.emitEvent(lease, "cancelled", payload)
	} else if err != nil || result.Status != "completed" {
		failure := err
		if failure == nil {
			failure = errors.New(result.Summary)
		}
		payload := terminalPayload("failed", result, usage)
		payload["failureFingerprint"] = FailureFingerprint("execution", failure)
		payload["error"] = failure.Error()
		loop.emitEvent(lease, "failed", payload)
	} else {
		payload := terminalPayload(result.Status, result, usage)
		loop.emitEvent(lease, "completed", payload)
	}
	loop.logger().Info("runner execution terminal", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation, "status", terminalStatus(cancelled, err, result))
	loop.Offers.Abandon(lease.JobID, lease.Generation)
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
		loop.logger().Warn("execution event queued for delivery after a transport failure", append(fields, "event_sequence", sequence)...)
		return
	}
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
		if text, isText := value.(string); isText {
			if shortened := truncateUTF8(text, maxTerminalPayloadFieldBytes); shortened != text {
				value = shortened + truncationMarker
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
// kind of unbounded detail the reduced payload exists to shed.
var minimalTerminalPayloadKeys = []string{"status", "exitCode", "error", "failureFingerprint", "durationMs", "branch", "wipBranch", "wipCommit", "wipPushed"}

// truncateUTF8 shortens value to at most limit bytes without splitting a rune.
func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
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
	if err != nil || result.Status != "completed" {
		return "failed"
	}
	return "completed"
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
	if status == "completed" && result.Raw != nil {
		payload["result"] = result.Raw
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
