//go:build integration

// These tests pin the fix for the bug where GracefulStop never returned while
// a runner's Connect stream (or a dashboard's StreamEvents stream) was open:
// both handlers only selected on their own stream context, which GracefulStop
// never cancels, so a connected runner — which holds Connect open
// indefinitely by design — blocked shutdown forever. Server.Shutdown gives
// both handlers a second signal to return on.
package server

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/secrethash"
	"google.golang.org/grpc/metadata"
)

// fakeConnectStream implements grpc.BidiStreamingServer[RunnerToOrchestrator,
// OrchestratorToRunner] without a real gRPC transport, so Connect can be
// driven and observed directly.
type fakeConnectStream struct {
	ctx  context.Context
	recv chan *runnerv1.RunnerToOrchestrator

	mu   sync.Mutex
	sent []*runnerv1.OrchestratorToRunner
}

func (f *fakeConnectStream) Send(message *runnerv1.OrchestratorToRunner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, message)
	return nil
}

func (f *fakeConnectStream) Recv() (*runnerv1.RunnerToOrchestrator, error) {
	message, ok := <-f.recv
	if !ok {
		return nil, io.EOF
	}
	return message, nil
}

func (f *fakeConnectStream) Context() context.Context     { return f.ctx }
func (f *fakeConnectStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeConnectStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeConnectStream) SetTrailer(metadata.MD)       {}
func (f *fakeConnectStream) SendMsg(m any) error          { return nil }
func (f *fakeConnectStream) RecvMsg(m any) error          { return nil }

// runnerWithCredential seeds an online runner and its credential row
// directly (rather than through the RegisterRunner RPC, which additionally
// needs a configured registration token), returning the runner id and the
// plaintext credential Connect's first message must present.
func (h *harness) runnerWithCredential() (runnerID, credential string) {
	h.t.Helper()
	runnerID, credential = idgen.NewID(), idgen.RandomSecret()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,now())`, runnerID, "runner-"+runnerID[:8])
	h.exec(`INSERT INTO app.runner_credentials(id,runner_id,credential_hash) VALUES($1,$2,$3)`, idgen.NewID(), runnerID, secrethash.HashSecret(credential))
	return runnerID, credential
}

// TestConnectReturnsOnShutdown pins that a runner control stream — held open
// indefinitely by design, since that is how a runner receives job offers —
// returns as soon as Server.Shutdown is called, instead of blocking until the
// runner disconnects on its own. That block is exactly what left
// grpc.Server.GracefulStop waiting forever in production, since GracefulStop
// itself never cancels an active stream's context.
func TestConnectReturnsOnShutdown(t *testing.T) {
	h := newHarness(t)
	runnerID, credential := h.runnerWithCredential()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream := &fakeConnectStream{ctx: ctx, recv: make(chan *runnerv1.RunnerToOrchestrator, 1)}
	stream.recv <- &runnerv1.RunnerToOrchestrator{RunnerId: runnerID, Credential: credential, Message: &runnerv1.RunnerToOrchestrator_Heartbeat{Heartbeat: &runnerv1.Heartbeat{Version: "1"}}}

	returned := make(chan error, 1)
	go func() { returned <- h.Connect(stream) }()

	// Give Connect a moment to authenticate and register the session, proving
	// it is genuinely parked in its select loop (the state a real runner
	// leaves it in) rather than having exited before Shutdown ran.
	waitFor(t, time.Second, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		_, connected := h.sessions[runnerID]
		return connected
	})

	h.Shutdown()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("Connect returned nil, want an Unavailable error telling the runner to reconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not return within 2s of Server.Shutdown; GracefulStop would hang here in production")
	}
}

// TestStreamEventsReturnsOnShutdown is StreamEvents' equivalent of
// TestConnectReturnsOnShutdown: a dashboard client that never disconnects on
// its own must not be able to block a graceful shutdown either. It drives
// StreamEvents directly (rather than through the startStream test helper)
// so it can observe the RPC's return value and timing itself instead of
// relying on a t.Cleanup-driven teardown.
func TestStreamEventsReturnsOnShutdown(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.adminContext())
	t.Cleanup(cancel)
	stream := &fakeEventStream{ctx: ctx}

	returned := make(chan error, 1)
	go func() { returned <- h.StreamEvents(&controlv1.StreamEventsRequest{}, stream) }()

	// Give StreamEvents time to finish its cold-connect catch-up and reach
	// the WaitForNotification call it would otherwise sit in until this
	// dashboard client disconnected on its own.
	time.Sleep(100 * time.Millisecond)

	h.Shutdown()

	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("StreamEvents returned nil, want a context-cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents did not return within 2s of Server.Shutdown; GracefulStop would hang here in production")
	}
}
