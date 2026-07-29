package main

import (
	"context"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/config"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/dispatch"
)

// streamWiringClient satisfies both dispatch.ControlClient and
// control.StreamSupervisorClient, which is the pair of roles the single
// *control.Client plays in run().
type streamWiringClient struct {
	mu           sync.Mutex
	drainReports []bool
}

func (client *streamWiringClient) AcceptOffer(string) error                  { return nil }
func (client *streamWiringClient) RejectOffer(string, string) error          { return nil }
func (client *streamWiringClient) RenewLease(string, int64, time.Time) error { return nil }
func (client *streamWiringClient) SendExecutionEvent(*runnerv1.ExecutionEvent) error {
	return nil
}
func (client *streamWiringClient) Receive() (*runnerv1.OrchestratorToRunner, error) {
	return nil, control.ErrNotConnected
}
func (client *streamWiringClient) Disconnect()                    {}
func (client *streamWiringClient) Connect(context.Context) error  { return nil }
func (client *streamWiringClient) Heartbeat([]string, bool) error { return nil }

func (client *streamWiringClient) SetDraining(draining bool) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.drainReports = append(client.drainReports, draining)
	return nil
}

func (client *streamWiringClient) reports() []bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]bool(nil), client.drainReports...)
}

type idleDispatcher struct{}

func (idleDispatcher) Execute(context.Context, control.Lease) (dispatch.Result, error) {
	return dispatch.Result{Status: "completed"}, nil
}

// The runner's drain state has to reach the orchestrator on every stream it
// establishes, which only happens if the supervisor's OnConnected hook is the
// control loop's Resume. Nothing else in the binary reports `draining: false`,
// so a supervisor wired to anything else strands the runner after a restart
// (issue #148).
func TestControlStreamSupervisorReportsTheRunnersDrainStateOnConnect(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		drained bool
	}{
		{name: "a restarted runner clears a stale drain flag"},
		{name: "a runner that is still draining re-asserts it", drained: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &streamWiringClient{}
			loop, err := dispatch.NewControlLoop(client, idleDispatcher{}, time.Now, time.Minute, 15*time.Second)
			if err != nil {
				t.Fatalf("NewControlLoop() error = %v", err)
			}
			if testCase.drained {
				loop.Drain()
			}
			settings := config.Config{Labels: []string{"linux"}, HeartbeatInterval: time.Second, ReconnectMin: time.Second, ReconnectMax: time.Minute}
			supervisor := controlStreamSupervisor(client, loop, settings, newReloadableStreamSettings(settings), func() {})
			if supervisor.OnConnected == nil {
				t.Fatal("the control stream supervisor has no OnConnected hook")
			}

			client.mu.Lock()
			client.drainReports = nil
			client.mu.Unlock()
			if err := supervisor.OnConnected(); err != nil {
				t.Fatalf("OnConnected() error = %v", err)
			}

			reports := client.reports()
			if len(reports) != 1 || reports[0] != loop.Draining() {
				t.Fatalf("drain states reported on connect = %v, want [%v]", reports, loop.Draining())
			}
		})
	}
}
