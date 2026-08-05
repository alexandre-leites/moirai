package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/taskpacket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type simulatedOrchestrator struct {
	runnerv1.UnimplementedRunnerControlServer
	offer            *runnerv1.JobOffer
	cancel           bool
	dropAfterStarted bool
	registered       chan *runnerv1.RegisterRunnerRequest
	messages         chan *runnerv1.RunnerToOrchestrator
	mu               sync.Mutex
	offerSent        bool
	leaseSent        bool
	cancelSent       bool
	connections      int
}

func (server *simulatedOrchestrator) RegisterRunner(_ context.Context, request *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
	server.registered <- request
	return &runnerv1.RegisterRunnerResponse{RunnerId: "runner-integration", Credential: "credential-integration"}, nil
}

func (server *simulatedOrchestrator) Connect(stream grpc.BidiStreamingServer[runnerv1.RunnerToOrchestrator, runnerv1.OrchestratorToRunner]) error {
	server.mu.Lock()
	server.connections++
	server.mu.Unlock()
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		server.messages <- message
		server.mu.Lock()
		sendOffer := message.GetHeartbeat() != nil && !server.offerSent
		if sendOffer {
			server.offerSent = true
		}
		sendLease := message.GetOfferAccepted() != nil && !server.leaseSent
		if sendLease {
			server.leaseSent = true
		}
		sendCancel := message.GetEvent() != nil && message.GetEvent().GetType() == "started" && server.cancel && !server.cancelSent
		if sendCancel {
			server.cancelSent = true
		}
		drop := message.GetEvent() != nil && message.GetEvent().GetType() == "started" && server.dropAfterStarted
		if drop {
			server.dropAfterStarted = false
		}
		server.mu.Unlock()
		if sendOffer {
			if err := stream.Send(&runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: server.offer}}); err != nil {
				return err
			}
		}
		if sendLease {
			if err := stream.Send(&runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: server.offer.GetJobId(), LeaseGeneration: server.offer.GetLeaseGeneration(), ExpiresAtUnixMs: time.Now().Add(time.Minute).UnixMilli()}}}); err != nil {
				return err
			}
		}
		if sendCancel {
			if err := stream.Send(&runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Cancel{Cancel: &runnerv1.CancelExecution{ExecutionId: "execution-integration", LeaseGeneration: 1}}}); err != nil {
				return err
			}
		}
		if drop {
			return status.Error(codes.Unavailable, "simulated stream interruption")
		}
	}
}

func TestRunnerAgainstSimulatedOrchestratorCompletesExecution(t *testing.T) {
	testRunnerAgainstSimulatedOrchestrator(t, false, false, "completed")
}

func TestRunnerAgainstSimulatedOrchestratorCancelsExecution(t *testing.T) {
	testRunnerAgainstSimulatedOrchestrator(t, true, false, "cancelled")
}

func TestRunnerAgainstSimulatedOrchestratorReconnectsAfterControlStreamInterruption(t *testing.T) {
	testRunnerAgainstSimulatedOrchestrator(t, false, true, "completed")
}

func testRunnerAgainstSimulatedOrchestrator(t *testing.T, cancel, dropAfterStarted bool, terminalType string) {
	t.Helper()
	repositoryURL := createRemoteRepository(t)
	dataDir := runnerDataDirectory(t)
	agent := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/sh
mkdir -p .loop
printf '{"protocolVersion":"1.0","executionId":"execution-integration","status":"completed","summary":"simulated execution","changedFiles":["runner-proof.txt"],"commandsRun":["agent.sh"]}' > .loop/result.json
printf simulated > runner-proof.txt
`
	if cancel {
		script += "sleep 30\n"
	} else if dropAfterStarted {
		script += "sleep 1\n"
	}
	if err := os.WriteFile(agent, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	packet, err := json.Marshal(taskpacket.Packet{ProtocolVersion: taskpacket.ProtocolVersion, JobID: "job-integration", ExecutionID: "execution-integration", Role: taskpacket.RoleDeveloper, Objective: "Create a proof file", Issue: taskpacket.Issue{ExternalID: "42", Title: "Integration", Body: "Exercise the runner"}, Repository: taskpacket.Repository{ProjectID: "project-integration", Mode: "managed_clone", URL: repositoryURL, DefaultBranch: "main", Branch: "agent/issue-42/run-1"}, PromptPath: ".loop/prompt.md", ExpectedOutput: ".loop/result.json", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	server := &simulatedOrchestrator{offer: &runnerv1.JobOffer{JobId: "job-integration", LeaseGeneration: 1, TaskPacketJson: string(packet)}, cancel: cancel, dropAfterStarted: dropAfterStarted, registered: make(chan *runnerv1.RegisterRunnerRequest, 1), messages: make(chan *runnerv1.RunnerToOrchestrator, 16)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	runnerv1.RegisterRunnerControlServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(func() { grpcServer.Stop(); listener.Close() })
	registrationTokenFile := filepath.Join(t.TempDir(), "registration-token")
	if err := os.WriteFile(registrationTokenFile, []byte("registration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", listener.Addr().String())
	t.Setenv("LOOP_RUNNER_DATA_DIR", dataDir)
	t.Setenv("LOOP_RUNNER_NAME", "runner-integration")
	t.Setenv("LOOP_RUNNER_REGISTRATION_TOKEN_FILE", registrationTokenFile)
	t.Setenv("LOOP_RUNNER_AGENT_BACKEND", "cli")
	t.Setenv("LOOP_RUNNER_AGENT_BINARY", agent)
	t.Setenv("LOOP_RUNNER_DOCKER_ENABLED", "false")
	t.Setenv("LOOP_RUNNER_HEARTBEAT_INTERVAL", "20ms")
	t.Setenv("LOOP_RUNNER_RECONNECT_MIN", "10ms")
	t.Setenv("LOOP_RUNNER_RECONNECT_MAX", "20ms")
	t.Setenv("LOOP_RUNNER_MINIMUM_FREE_BYTES", "1")
	t.Setenv("LOOP_RUNNER_RETAIN_WORKSPACES", "succeeded,failed,abandoned")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	runResult := make(chan error, 1)
	go func() { runResult <- run(ctx) }()
	registered := receiveRegistration(t, server.registered, runResult)
	if registered.GetToken() != "registration-token" || registered.GetName() != "runner-integration" || registered.GetProtocolVersion() != "1.0" {
		t.Fatalf("registration = %#v", registered)
	}
	events := receiveTerminalEvents(t, server.messages, terminalType)
	if events[0].GetType() != "started" || events[0].GetEventSequence() != 1 || events[1].GetType() != terminalType || events[1].GetEventSequence() != 2 {
		result, _ := os.ReadFile(filepath.Join(dataDir, "workspaces", "job-job-integration", "repository", ".loop", "result.json"))
		stderr, _ := os.ReadFile(filepath.Join(dataDir, "workspaces", "job-job-integration", "repository", ".loop", "cli.stderr.log"))
		t.Fatalf("events = %s/%d, %s/%d payload=%s result=%s stderr=%s", events[0].GetType(), events[0].GetEventSequence(), events[1].GetType(), events[1].GetEventSequence(), events[1].GetPayloadJson(), result, stderr)
	}
	if dropAfterStarted {
		server.mu.Lock()
		connections := server.connections
		server.mu.Unlock()
		if connections < 2 {
			t.Fatalf("control stream connections = %d, want at least 2", connections)
		}
	}
	if terminalType == "completed" {
		proof := filepath.Join(dataDir, "workspaces", "job-job-integration", "repository", "runner-proof.txt")
		if _, err := os.Stat(proof); err != nil {
			t.Fatalf("runner worktree proof missing: %v", err)
		}
	}
	stop()
	select {
	case err := <-runResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not stop")
	}
}

func receiveRegistration(t *testing.T, registrations <-chan *runnerv1.RegisterRunnerRequest, runResult <-chan error) *runnerv1.RegisterRunnerRequest {
	t.Helper()
	select {
	case registration := <-registrations:
		return registration
	case err := <-runResult:
		t.Fatalf("runner stopped before registration: %v", err)
		return nil
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not register")
		return nil
	}
}

func receiveTerminalEvents(t *testing.T, messages <-chan *runnerv1.RunnerToOrchestrator, terminalType string) []*runnerv1.ExecutionEvent {
	t.Helper()
	var events []*runnerv1.ExecutionEvent
	deadline := time.After(5 * time.Second)
	for len(events) < 2 {
		select {
		case message := <-messages:
			if event := message.GetEvent(); event != nil {
				events = append(events, event)
				if event.GetType() == terminalType && len(events) == 2 {
					return events
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s terminal event; events = %#v", terminalType, events)
		}
	}
	return events
}

func runnerDataDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/dev/shm", "moirai-runner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func createRemoteRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	working := filepath.Join(root, "working")
	remote := filepath.Join(root, "remote.git")
	runGit(t, root, "init", "-b", "main", working)
	if err := os.WriteFile(filepath.Join(working, "README.md"), []byte("runner integration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, working, "add", "README.md")
	runGit(t, working, "-c", "user.email=test@example.invalid", "-c", "user.name=Runner Test", "commit", "-m", "initial")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, working, "remote", "add", "origin", remote)
	runGit(t, working, "push", "origin", "main")
	return remote
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}
