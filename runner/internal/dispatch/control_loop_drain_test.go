package dispatch

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
)

// assertDrainReports checks both what the orchestrator observed and that the
// runner's own state is what it observed: `app.runners.draining` is a mirror of
// ControlLoop.Draining(), and a report that does not match it is the defect
// issue #148 is about.
func assertDrainReports(t *testing.T, client *loopClient, loop *ControlLoop, want []bool) {
	t.Helper()
	reports := client.reportedDrainStates()
	if !slices.Equal(reports, want) {
		t.Fatalf("reported drain states = %v, want %v", reports, want)
	}
	if len(reports) == 0 {
		return
	}
	if last := reports[len(reports)-1]; last != loop.Draining() {
		t.Fatalf("last reported drain state = %v, want ControlLoop.Draining() = %v", last, loop.Draining())
	}
}

func newDrainTestLoop(t *testing.T, client ControlClient) *ControlLoop {
	t.Helper()
	loop, err := NewControlLoopWithOutbox(client, &staticDispatcher{}, time.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	return loop
}

func TestControlLoopResumeReportsTheDrainStateTheRunnerActuallyHas(t *testing.T) {
	client := &loopClient{}
	loop := newDrainTestLoop(t, client)

	// A fresh process is not draining, and says so on its first stream. This is
	// what clears a `draining = true` left behind by a previous incarnation of
	// the same runner identity.
	if err := loop.Resume(); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}
	assertDrainReports(t, client, loop, []bool{false})

	loop.Drain()
	assertDrainReports(t, client, loop, []bool{false, true})

	// A drain is reported once, not once per call.
	loop.Drain()
	assertDrainReports(t, client, loop, []bool{false, true})

	// Every later stream re-asserts the drain rather than clearing it: a runner
	// that is genuinely still draining must never report `false`.
	if err := loop.Resume(); err != nil {
		t.Fatalf("Resume() after drain error = %v", err)
	}
	if err := loop.Resume(); err != nil {
		t.Fatalf("second Resume() after drain error = %v", err)
	}
	assertDrainReports(t, client, loop, []bool{false, true, true, true})
}

// Once the runner is draining, no report may say otherwise — including one from
// a reconnect that was already under way when the drain landed. Reading the
// state and sending it are a single critical section, so a reporter that queued
// behind an in-flight report re-reads the state instead of sending the value it
// sampled before it queued.
//
// The interleaving is forced, not hoped for. Holding drainReports stands in for
// a report already on the wire; the barrier below waits for the queued resume to
// actually park on that lock before the drain flips the flag, so the test cannot
// degrade into a vacuous pass on a loaded machine. Both reports are then
// released against a draining runner, so the expectation holds whichever wins
// the lock.
func TestControlLoopResumeCannotReportAStaleDrainState(t *testing.T) {
	client := &loopClient{}
	loop := newDrainTestLoop(t, client)

	loop.drainReports.Lock()
	resumeErr := make(chan error, 1)
	go func() { resumeErr <- loop.Resume() }()
	if !waitForQueuedDrainReport(time.Now().Add(5 * time.Second)) {
		loop.drainReports.Unlock()
		t.Fatal("timed out waiting for a resume to queue behind the report lock")
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		loop.Drain()
	}()
	if !waitForDraining(loop, time.Now().Add(5*time.Second)) {
		loop.drainReports.Unlock()
		t.Fatal("timed out waiting for the runner to start draining")
	}

	loop.drainReports.Unlock()
	select {
	case err := <-resumeErr:
		if err != nil {
			t.Fatalf("queued Resume() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the queued resume never completed")
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the concurrent drain never completed")
	}

	// A reporter that sampled the state before queueing would report the stale
	// `false` here.
	assertDrainReports(t, client, loop, []bool{true, true})
}

// waitForQueuedDrainReport reports whether a goroutine has parked inside
// reportDrainState. That is the only observable point at which a queued
// reporter has provably reached the lock — a sleep would leave the test passing
// without ever producing the interleaving it exists to cover.
func waitForQueuedDrainReport(deadline time.Time) bool {
	stacks := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		if bytes.Contains(stacks[:runtime.Stack(stacks, true)], []byte("(*ControlLoop).reportDrainState")) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func waitForDraining(loop *ControlLoop, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if loop.Draining() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// The orchestrator should learn the runner is unavailable before the runner
// spends the fresh stream on a backlog of buffered events. A resume that cannot
// deliver that backlog must still fail, as the bare FlushEvents hook it replaced
// did, so the supervisor drops the stream and retries.
func TestControlLoopResumeReportsDrainStateBeforeFlushingBufferedEvents(t *testing.T) {
	now := time.Now()
	transportErr := errors.New("control stream disconnected")
	client := &loopClient{sendErr: transportErr}
	dispatcher := &blockingDispatcher{started: make(chan struct{}), cancelled: make(chan control.Lease, 1)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, func() time.Time { return now }, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoop() error = %v", err)
	}
	offer := loopOffer(t)
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}); err != nil {
		t.Fatalf("Handle(offer) error = %v", err)
	}
	acknowledgement := &runnerv1.LeaseAcknowledged{JobId: offer.GetJobId(), LeaseGeneration: offer.GetLeaseGeneration(), ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}
	if err := loop.Handle(context.Background(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: acknowledgement}}); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not start")
	}

	// The drain report gets through but the buffered event does not.
	if err := loop.Resume(); !errors.Is(err, transportErr) {
		t.Fatalf("Resume() error = %v, want %v", err, transportErr)
	}

	client.mu.Lock()
	client.sendErr = nil
	client.mu.Unlock()
	if err := loop.Resume(); err != nil {
		t.Fatalf("Resume() error = %v", err)
	}

	client.mu.Lock()
	sends := append([]string(nil), client.sends...)
	client.mu.Unlock()
	if !slices.Equal(sends, []string{"draining:false", "draining:false", "event:started"}) {
		t.Fatalf("stream sends = %v, want each drain report before the buffered event", sends)
	}

	loop.Cancel("execution-1", 1)
	waitForEvents(t, client, 2)
}

// A resume that cannot report the drain state must fail, so StreamSupervisor
// drops the stream and retries. Carrying on would leave the orchestrator with a
// stale view of the runner for the whole life of the connection.
func TestControlLoopResumeFailsWhenTheDrainReportCannotBeDelivered(t *testing.T) {
	client := &loopClient{drainErr: control.ErrNotConnected}
	loop := newDrainTestLoop(t, client)

	err := loop.Resume()
	if !errors.Is(err, control.ErrNotConnected) {
		t.Fatalf("Resume() error = %v, want %v", err, control.ErrNotConnected)
	}
	assertDrainReports(t, client, loop, nil)
}

// supervisedLoopClient is a ControlClient that also satisfies
// control.StreamSupervisorClient, so a test can drive the real supervisor over
// the same wiring runner/cmd/runner/main.go uses.
type supervisedLoopClient struct {
	*loopClient
	onHeartbeat func()
}

func (client *supervisedLoopClient) Connect(context.Context) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.sends = append(client.sends, "connect")
	return nil
}

func (client *supervisedLoopClient) Heartbeat([]string, bool) error {
	if client.onHeartbeat != nil {
		client.onHeartbeat()
	}
	return nil
}

// The end-to-end shape of issue #148: whatever the runner's drain state is when
// a control stream comes up, that is what the orchestrator is told, and it is
// told before the first heartbeat.
func TestStreamSupervisorReportsTheRunnersDrainStateOnConnect(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		drained bool
	}{
		{name: "a restarted runner clears a stale drain flag"},
		{name: "a runner that is still draining re-asserts it", drained: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &supervisedLoopClient{loopClient: &loopClient{}}
			loop := newDrainTestLoop(t, client)
			if testCase.drained {
				loop.Drain()
			}
			// Forget anything the previous stream carried; only what this
			// connect reports is under test.
			client.mu.Lock()
			client.drainReports = nil
			client.sends = nil
			client.mu.Unlock()
			client.onHeartbeat = cancel

			err := control.StreamSupervisor{
				Client:            client,
				HeartbeatInterval: time.Hour,
				ReconnectMin:      time.Millisecond,
				ReconnectMax:      time.Millisecond,
				Busy:              loop.Busy,
				OnConnected:       loop.Resume,
			}.Run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v, want context cancellation", err)
			}

			assertDrainReports(t, client.loopClient, loop, []bool{testCase.drained})
			client.mu.Lock()
			sends := append([]string(nil), client.sends...)
			client.mu.Unlock()
			if len(sends) != 2 || sends[0] != "connect" {
				t.Fatalf("stream sends = %v, want the drain report on a connected stream", sends)
			}
		})
	}
}
