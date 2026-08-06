package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestSupervisorExecutesCommand(t *testing.T) {
	workspace := t.TempDir()
	supervisor := NewSupervisor()
	var output bytes.Buffer
	result, err := supervisor.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "printf '%s' \"$LOOP_TEST_VALUE\""},
		Environment: map[string]string{"LOOP_TEST_VALUE": "expected"},
		Timeout:     time.Second,
	}, &output, &output)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if output.String() != "expected" {
		t.Fatalf("output = %q, want expected", output.String())
	}
}

func TestSupervisorDoesNotInheritRunnerEnvironment(t *testing.T) {
	t.Setenv("RUNNER_SECRET", "must-not-reach-agent")
	workspace := t.TempDir()
	var output bytes.Buffer
	_, err := NewSupervisor().Execute(context.Background(), Request{
		ExecutionID: "execution-isolated-environment",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "printf '%s:%s' \"$RUNNER_SECRET\" \"$LOOP_TEST_VALUE\""},
		Environment: map[string]string{"LOOP_TEST_VALUE": "allowed"},
		Timeout:     time.Second,
	}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != ":allowed" {
		t.Fatalf("agent environment = %q", output.String())
	}
}

func TestSupervisorCreatesWorkspaceTemporaryDirectory(t *testing.T) {
	workspace := t.TempDir()
	_, err := NewSupervisor().Execute(context.Background(), Request{ExecutionID: "execution-tmpdir", Workspace: workspace, Command: []string{"/bin/sh", "-c", "test -d \"$TMPDIR\""}, Timeout: time.Second}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorReportsStartedProcessID(t *testing.T) {
	pid := 0
	_, err := NewSupervisor().Execute(context.Background(), Request{ExecutionID: "execution-pid", Workspace: t.TempDir(), Command: []string{"true"}, Timeout: time.Second, OnStarted: func(value int) { pid = value }}, nil, nil)
	if err != nil || pid < 1 {
		t.Fatalf("Execute() = %v, pid = %d", err, pid)
	}
}

// #276: a zero timeout used to mean "no deadline," which is exactly how a
// wedged agent process ended up holding its project lock forever. A valid
// task packet can no longer produce one, so this is now rejected rather than
// silently run without a deadline.
func TestSupervisorRejectsZeroTimeout(t *testing.T) {
	_, err := NewSupervisor().Execute(context.Background(), Request{
		ExecutionID: "execution-zero-timeout",
		Workspace:   t.TempDir(),
		Command:     []string{"true"},
		Timeout:     0,
	}, nil, nil)
	if err == nil {
		t.Fatal("Execute() accepted a zero timeout")
	}
}

func TestSupervisorRejectsNegativeTimeout(t *testing.T) {
	_, err := NewSupervisor().Execute(context.Background(), Request{
		ExecutionID: "execution-negative-timeout",
		Workspace:   t.TempDir(),
		Command:     []string{"true"},
		Timeout:     -1,
	}, nil, nil)
	if err == nil {
		t.Fatal("Execute() accepted a negative timeout")
	}
}

func TestSupervisorTimeoutTerminatesProcess(t *testing.T) {
	workspace := t.TempDir()
	supervisor := NewSupervisor()
	started := time.Now()
	result, err := supervisor.Execute(context.Background(), Request{
		ExecutionID: "execution-timeout",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "sleep 10"},
		Timeout:     25 * time.Millisecond,
	}, os.Stdout, os.Stderr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute() error = %v, want deadline exceeded", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("ExitCode = 0 after timeout")
	}
	if time.Since(started) > time.Second {
		t.Fatal("timed out process was not terminated promptly")
	}
}

// #410: an agent that stops producing output but never exits used to hold the
// execution until the whole packet timeout elapsed. The silence bound
// terminates it instead, so the goal gate can re-engage the agent.
func TestSupervisorSilenceTerminatesSilentProcess(t *testing.T) {
	workspace := t.TempDir()
	supervisor := NewSupervisor()
	started := time.Now()
	result, err := supervisor.Execute(context.Background(), Request{
		ExecutionID: "execution-silence",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "sleep 10"},
		Timeout:     time.Minute,
		Silence:     50 * time.Millisecond,
	}, os.Stdout, os.Stderr)
	if !errors.Is(err, ErrSilenceExceeded) {
		t.Fatalf("Execute() error = %v, want silence exceeded", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("ExitCode = 0 after silence termination")
	}
	if time.Since(started) > time.Second {
		t.Fatal("silent process was not terminated promptly")
	}
}

// A process that keeps writing output is working, not wedged, and must not be
// terminated for silence however long it runs.
func TestSupervisorOutputResetsTheSilenceClock(t *testing.T) {
	workspace := t.TempDir()
	supervisor := NewSupervisor()
	result, err := supervisor.Execute(context.Background(), Request{
		ExecutionID: "execution-silence-reset",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "i=0; while [ $i -lt 10 ]; do echo tick; i=$((i+1)); sleep 0.02; done"},
		Timeout:     time.Minute,
		Silence:     100 * time.Millisecond,
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a clean exit", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestSupervisorCancelTerminatesActiveExecution(t *testing.T) {
	workspace := t.TempDir()
	supervisor := NewSupervisor()
	done := make(chan error, 1)
	go func() {
		_, err := supervisor.Execute(context.Background(), Request{
			ExecutionID: "execution-cancel",
			Workspace:   workspace,
			Command:     []string{"/bin/sh", "-c", "sleep 10"},
			Timeout:     time.Minute,
		}, os.Stdout, os.Stderr)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		err := supervisor.Cancel("execution-cancel")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution did not become active: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err == nil {
		t.Fatal("Execute() error = nil after cancellation")
	}
}
