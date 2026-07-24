package control

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeSupervisorClient struct {
	mu              sync.Mutex
	connectErrors   []error
	heartbeatErrors []error
	connects        int
	disconnects     int
	heartbeats      int
	busy            []bool
	labels          [][]string
	onHeartbeat     func()
}

func (c *fakeSupervisorClient) Connect(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connects++
	if len(c.connectErrors) == 0 {
		return nil
	}
	err := c.connectErrors[0]
	c.connectErrors = c.connectErrors[1:]
	return err
}

func (c *fakeSupervisorClient) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnects++
}

func (c *fakeSupervisorClient) Heartbeat(labels []string, busy bool) error {
	c.mu.Lock()
	c.heartbeats++
	c.busy = append(c.busy, busy)
	c.labels = append(c.labels, append([]string(nil), labels...))
	var err error
	if len(c.heartbeatErrors) > 0 {
		err = c.heartbeatErrors[0]
		c.heartbeatErrors = c.heartbeatErrors[1:]
	}
	onHeartbeat := c.onHeartbeat
	c.mu.Unlock()
	if onHeartbeat != nil {
		onHeartbeat()
	}
	return err
}

func TestStreamSupervisorReconnectsThenSendsInitialHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{connectErrors: []error{errors.New("unavailable")}}
	sleeps := make([]time.Duration, 0, 1)
	client.onHeartbeat = cancel
	supervisor := StreamSupervisor{
		Client:            client,
		Labels:            []string{"linux"},
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      4 * time.Second,
		Jitter:            func(duration time.Duration) time.Duration { return duration },
		Sleep: func(_ context.Context, duration time.Duration) error {
			sleeps = append(sleeps, duration)
			return nil
		},
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if client.connects != 2 || client.heartbeats != 1 || client.disconnects != 1 {
		t.Fatalf("Run() calls = connects %d, heartbeats %d, disconnects %d", client.connects, client.heartbeats, client.disconnects)
	}
	if len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("Run() retries = %#v", sleeps)
	}
}

func TestStreamSupervisorResumesEventsAndReportsBusyState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{}
	resumed := 0
	client.onHeartbeat = cancel
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
		Busy:              func() bool { return true },
		OnConnected: func() error {
			resumed++
			return nil
		},
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if resumed != 1 {
		t.Fatalf("OnConnected calls = %d, want 1", resumed)
	}
	if len(client.busy) != 1 || !client.busy[0] {
		t.Fatalf("heartbeat busy states = %#v", client.busy)
	}
}

func TestStreamSupervisorReconcilesBeforeHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{}
	client.onHeartbeat = cancel
	reconciled := 0
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
		OnHeartbeat: func() error {
			reconciled++
			return nil
		},
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if reconciled != 1 || client.heartbeats != 1 {
		t.Fatalf("reconciliations = %d, heartbeats = %d", reconciled, client.heartbeats)
	}
}

func TestStreamSupervisorReadsDynamicLabels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{}
	client.onHeartbeat = cancel
	labels := []string{"linux"}
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
		Settings: func() StreamSettings {
			return StreamSettings{Labels: append([]string(nil), labels...)}
		},
	}
	labels = []string{"linux", "docker"}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(client.labels) != 1 || len(client.labels[0]) != 2 || client.labels[0][1] != "docker" {
		t.Fatalf("heartbeat labels = %#v", client.labels)
	}
}

func TestStreamSupervisorAppliesReloadedReconnectBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{connectErrors: []error{errors.New("unavailable"), errors.New("unavailable")}}
	settings := StreamSettings{HeartbeatInterval: time.Hour, ReconnectMin: time.Second, ReconnectMax: 4 * time.Second}
	var sleeps []time.Duration
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      4 * time.Second,
		Jitter:            func(duration time.Duration) time.Duration { return duration },
		Settings:          func() StreamSettings { return settings },
		Sleep: func(_ context.Context, duration time.Duration) error {
			sleeps = append(sleeps, duration)
			if len(sleeps) == 1 {
				settings = StreamSettings{HeartbeatInterval: time.Hour, ReconnectMin: 3 * time.Second, ReconnectMax: 3 * time.Second}
				return nil
			}
			cancel()
			return context.Canceled
		},
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 3*time.Second {
		t.Fatalf("reload reconnect delays = %#v", sleeps)
	}
}

func TestStreamSupervisorStopsOnPermanentControlFailure(t *testing.T) {
	client := &fakeSupervisorClient{connectErrors: []error{status.Error(codes.Unauthenticated, "credential rejected")}}
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Second,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
		Sleep: func(context.Context, time.Duration) error {
			t.Fatal("Run() retried a permanent control failure")
			return nil
		},
	}
	if err := supervisor.Run(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Run() error = %v, want unauthenticated", err)
	}
	if client.connects != 1 || client.disconnects != 0 {
		t.Fatalf("Run() calls = connects %d, disconnects %d", client.connects, client.disconnects)
	}
}

func TestStreamSupervisorSendsPeriodicHeartbeats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeSupervisorClient{}
	var heartbeats atomic.Int32
	client.onHeartbeat = func() {
		if heartbeats.Add(1) == 2 {
			cancel()
		}
	}
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Millisecond,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if got := client.heartbeats; got < 2 {
		t.Fatalf("Run() heartbeats = %d, want at least 2", got)
	}
}

func TestStreamSupervisorDisconnectsAfterInitialHeartbeatFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeSupervisorClient{heartbeatErrors: []error{errors.New("stream closed")}}
	supervisor := StreamSupervisor{
		Client:            client,
		HeartbeatInterval: time.Hour,
		ReconnectMin:      time.Second,
		ReconnectMax:      time.Second,
		Jitter:            func(duration time.Duration) time.Duration { return duration },
		Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	}
	if err := supervisor.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context cancellation", err)
	}
	if client.connects != 1 || client.heartbeats != 1 || client.disconnects != 1 {
		t.Fatalf("Run() calls = connects %d, heartbeats %d, disconnects %d", client.connects, client.heartbeats, client.disconnects)
	}
}

func TestStreamSupervisorRejectsInvalidConfigurationAndCapsBackoff(t *testing.T) {
	if err := (StreamSupervisor{}).Run(context.Background()); err == nil {
		t.Fatal("Run() accepted an empty supervisor")
	}
	if got := nextBackoff(2*time.Second, 3*time.Second); got != 3*time.Second {
		t.Fatalf("nextBackoff() = %s, want 3s", got)
	}
	if got := jitterDelay(func(time.Duration) time.Duration { return 10 * time.Second }, time.Second, 3*time.Second); got != 3*time.Second {
		t.Fatalf("jitterDelay() = %s, want 3s", got)
	}
}
