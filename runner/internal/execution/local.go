package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Request struct {
	ExecutionID string
	Workspace   string
	Command     []string
	Environment map[string]string
	Timeout     time.Duration
	OnStarted   func(int)
}

type Result struct {
	ExitCode int
	Started  time.Time
	Finished time.Time
}

type Supervisor struct {
	mu        sync.Mutex
	processes map[string]*exec.Cmd
}

func NewSupervisor() *Supervisor {
	return &Supervisor{processes: make(map[string]*exec.Cmd)}
}

func (supervisor *Supervisor) Execute(
	parent context.Context,
	request Request,
	stdout io.Writer,
	stderr io.Writer,
) (Result, error) {
	if request.ExecutionID == "" {
		return Result{}, errors.New("execution ID is required")
	}
	if len(request.Command) == 0 || request.Command[0] == "" {
		return Result{}, errors.New("command is required")
	}
	if request.Workspace == "" {
		return Result{}, errors.New("workspace is required")
	}
	if request.Timeout <= 0 {
		return Result{}, errors.New("timeout must be positive")
	}

	workspace := filepath.Clean(request.Workspace)
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("workspace is not a directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Dir = workspace
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = environment(request.Environment, workspace)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	started := time.Now().UTC()
	if err := command.Start(); err != nil {
		return Result{Started: started, Finished: time.Now().UTC()}, fmt.Errorf("start command: %w", err)
	}
	if request.OnStarted != nil {
		request.OnStarted(command.Process.Pid)
	}
	if err := supervisor.track(request.ExecutionID, command); err != nil {
		_ = terminate(command)
		_ = command.Wait()
		return Result{Started: started, Finished: time.Now().UTC()}, err
	}
	defer supervisor.untrack(request.ExecutionID)

	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()

	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		_ = terminate(command)
		select {
		case waitErr = <-wait:
		case <-time.After(5 * time.Second):
			_ = kill(command)
			waitErr = <-wait
		}
	}

	result := Result{ExitCode: exitCode(waitErr), Started: started, Finished: time.Now().UTC()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if waitErr != nil {
		return result, fmt.Errorf("command exited unsuccessfully: %w", waitErr)
	}
	return result, nil
}

func (supervisor *Supervisor) Cancel(executionID string) error {
	supervisor.mu.Lock()
	command := supervisor.processes[executionID]
	supervisor.mu.Unlock()
	if command == nil || command.Process == nil {
		return fmt.Errorf("execution %q is not active", executionID)
	}
	return terminate(command)
}

func (supervisor *Supervisor) track(executionID string, command *exec.Cmd) error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if _, exists := supervisor.processes[executionID]; exists {
		return fmt.Errorf("execution %q is already active", executionID)
	}
	supervisor.processes[executionID] = command
	return nil
}

func (supervisor *Supervisor) untrack(executionID string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	delete(supervisor.processes, executionID)
}

func environment(overrides map[string]string, workspace string) []string {
	environment := []string{
		"HOME=" + workspace,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR=" + filepath.Join(workspace, ".loop", "tmp"),
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func terminate(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
}

func kill(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}
