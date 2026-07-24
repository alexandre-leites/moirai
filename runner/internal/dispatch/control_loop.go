package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
)

const defaultEventBufferSize = 128

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

type activeExecution struct {
	lease     control.Lease
	cancel    context.CancelFunc
	cancelled bool
	terminal  bool
}

type ControlLoop struct {
	Client     ControlClient
	Offers     *control.OfferState
	Reporter   *control.EventReporter
	Dispatcher executionDispatcher
	Logger     *slog.Logger

	mu       sync.Mutex
	draining bool
	active   *activeExecution
}

func NewControlLoop(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration) (*ControlLoop, error) {
	return NewControlLoopWithRedaction(client, dispatcher, now, leaseDuration, renewalLead, nil)
}

func NewControlLoopWithRedaction(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string) (*ControlLoop, error) {
	return NewControlLoopWithOutbox(client, dispatcher, now, leaseDuration, renewalLead, redactionPrefixes, "")
}

func NewControlLoopWithOutbox(client ControlClient, dispatcher executionDispatcher, now func() time.Time, leaseDuration, renewalLead time.Duration, redactionPrefixes []string, outboxPath string) (*ControlLoop, error) {
	if client == nil || now == nil {
		return nil, errors.New("runner control loop dependencies are required")
	}
	offers, err := control.NewOfferState(client, now, leaseDuration, renewalLead)
	if err != nil {
		return nil, err
	}
	reporter, err := control.NewEventReporterWithOutbox(client, defaultEventBufferSize, redactionPrefixes, outboxPath)
	if err != nil {
		return nil, err
	}
	return &ControlLoop{Client: client, Offers: offers, Reporter: reporter, Dispatcher: dispatcher}, nil
}

func (loop *ControlLoop) Run(ctx context.Context) error {
	if err := loop.validate(); err != nil {
		return err
	}
	for {
		message, err := loop.Client.Receive()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			loop.Client.Disconnect()
			if err := waitForControlReconnect(ctx); err != nil {
				return err
			}
			continue
		}
		if err := loop.Handle(ctx, message); err != nil {
			return err
		}
	}
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
			return loop.Client.RejectOffer(offer.GetJobId(), "runner is draining")
		}
		_, err := loop.Offers.Admit(offer)
		return err
	}
	if cancellation := message.GetCancel(); cancellation != nil {
		loop.logger().Info("runner received execution cancellation", "execution_id", cancellation.GetExecutionId(), "lease_generation", cancellation.GetLeaseGeneration())
		loop.Cancel(cancellation.GetExecutionId(), cancellation.GetLeaseGeneration())
		return nil
	}
	if acknowledgement := message.GetLeaseAcknowledged(); acknowledgement != nil {
		_, alreadyActive := loop.Offers.ActiveLease()
		if !loop.Offers.ApplyAcknowledgement(acknowledgement) {
			return nil
		}
		if alreadyActive {
			return nil
		}
		lease, ok := loop.Offers.ActiveLease()
		if !ok {
			return errors.New("acknowledged lease is unavailable")
		}
		if err := loop.Reporter.Begin(lease); err != nil {
			loop.Offers.Abandon(lease.JobID, lease.Generation)
			return fmt.Errorf("begin execution event reporting: %w", err)
		}
		executionContext, cancel := context.WithCancel(ctx)
		loop.mu.Lock()
		loop.active = &activeExecution{lease: lease, cancel: cancel}
		loop.mu.Unlock()
		loop.logger().Info("runner execution acknowledged", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation)
		go loop.execute(executionContext, lease)
		return nil
	}
	return errors.New("orchestrator control message is unsupported")
}

func (loop *ControlLoop) Drain() {
	if loop == nil {
		return
	}
	loop.mu.Lock()
	loop.draining = true
	loop.mu.Unlock()
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
	active := loop.active
	if active == nil || active.terminal || active.lease.Packet.ExecutionID != executionID || active.lease.Generation != generation {
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
	if expired := loop.Offers.Expire(); expired != nil {
		loop.logger().Warn("runner lease expired", "job_id", expired.JobID, "execution_id", expired.Packet.ExecutionID, "lease_generation", expired.Generation)
		loop.cancelExpired(*expired)
		loop.Reporter.Abandon(expired.JobID, expired.Generation)
		return nil
	}
	if _, err := loop.Offers.RenewDue(); err != nil {
		return fmt.Errorf("renew active lease: %w", err)
	}
	return nil
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

func (loop *ControlLoop) Busy() bool {
	if loop == nil || loop.Offers == nil {
		return false
	}
	_, active := loop.Offers.ActiveLease()
	return active
}

func (loop *ControlLoop) execute(ctx context.Context, lease control.Lease) {
	started := time.Now()
	loop.logger().Info("runner execution started", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation)
	_, _ = loop.Reporter.Emit(lease.JobID, lease.Generation, "started", map[string]any{"status": "running"})
	result, err := loop.Dispatcher.Execute(ctx, lease)
	usage := executionUsage(started, result)
	cancelled := loop.finish(lease) || errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	if cancelled {
		payload := terminalPayload("cancelled", result, usage)
		_, _ = loop.Reporter.Emit(lease.JobID, lease.Generation, "cancelled", payload)
	} else if err != nil || result.Status != "completed" {
		failure := err
		if failure == nil {
			failure = errors.New(result.Summary)
		}
		payload := terminalPayload("failed", result, usage)
		payload["failureFingerprint"] = FailureFingerprint("execution", failure)
		payload["error"] = failure.Error()
		_, _ = loop.Reporter.Emit(lease.JobID, lease.Generation, "failed", payload)
	} else {
		payload := terminalPayload(result.Status, result, usage)
		_, _ = loop.Reporter.Emit(lease.JobID, lease.Generation, "completed", payload)
	}
	loop.logger().Info("runner execution terminal", "job_id", lease.JobID, "execution_id", lease.Packet.ExecutionID, "lease_generation", lease.Generation, "status", terminalStatus(cancelled, err, result))
	loop.Offers.Abandon(lease.JobID, lease.Generation)
	loop.Reporter.Finish(lease.JobID, lease.Generation)
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
		"status":       status,
		"exitCode":     result.ExitCode,
		"changedFiles": result.ChangedFiles,
		"commandsRun":  result.CommandsRun,
	}
	for key, value := range usage {
		payload[key] = value
	}
	return payload
}

func (loop *ControlLoop) finish(lease control.Lease) bool {
	loop.mu.Lock()
	defer loop.mu.Unlock()
	if loop.active == nil || loop.active.lease.JobID != lease.JobID || loop.active.lease.Generation != lease.Generation {
		return false
	}
	loop.active.terminal = true
	cancelled := loop.active.cancelled
	loop.active = nil
	return cancelled
}

func (loop *ControlLoop) cancelExpired(lease control.Lease) {
	loop.mu.Lock()
	active := loop.active
	if active == nil || active.lease.JobID != lease.JobID || active.lease.Generation != lease.Generation {
		loop.mu.Unlock()
		return
	}
	active.cancelled = true
	active.terminal = true
	active.cancel()
	loop.active = nil
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

func waitForControlReconnect(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
