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
	// Silence bounds how long the process may run without writing anything to
	// stdout or stderr. Zero disables the bound. It is the runner's answer to an
	// agent that stops talking but never exits: instead of waiting out the whole
	// timeout on a wedged process, the process is terminated once it has been
	// silent for this long, and the goal gate's continuation loop re-engages it.
	Silence   time.Duration
	OnStarted func(int)
}

type Result struct {
	ExitCode int
	Started  time.Time
	Finished time.Time
}

// ErrSilenceExceeded reports a process that produced no output for the
// execution's silence bound. It is the runner's own signal that the agent is
// wedged rather than working, distinct from a timeout so the goal gate and the
// terminal payload can tell "ran out of wall-clock time" from "stopped
// talking". The text is deliberately free of paths and timestamps so the
// failure fingerprint stays stable across executions.
var ErrSilenceExceeded = errors.New("agent produced no output for too long")

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
	// A valid task packet (see taskpacket.go's Validate) never produces a
	// non-positive timeout, so this rejects rather than silently running an
	// execution with no deadline (#276) -- the loud failure here is a defensive
	// backstop against a caller bypassing that validation, not a path a real
	// packet should ever reach.
	if request.Timeout <= 0 {
		return Result{}, errors.New("timeout must be positive")
	}

	workspace := filepath.Clean(request.Workspace)
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("workspace is not a directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".loop", "tmp"), 0o700); err != nil {
		return Result{}, fmt.Errorf("create workspace temporary directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()
	command := exec.Command(request.Command[0], request.Command[1:]...)
	command.Dir = workspace
	var silence *silenceWatch
	if request.Silence > 0 {
		silence = newSilenceWatch(request.Silence)
		stdout = silence.wrap(stdout)
		stderr = silence.wrap(stderr)
	}
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = MinimalEnvironment(request.Environment, workspace)
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

	var silenceExpired <-chan struct{}
	if silence != nil {
		go silence.run()
		defer silence.close()
		silenceExpired = silence.expired()
	}

	var waitErr error
	var silent bool
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
	case <-silenceExpired:
		silent = true
		_ = terminate(command)
		select {
		case waitErr = <-wait:
		case <-time.After(5 * time.Second):
			_ = kill(command)
			waitErr = <-wait
		}
	}

	result := Result{ExitCode: exitCode(waitErr), Started: started, Finished: time.Now().UTC()}
	if silent {
		return result, ErrSilenceExceeded
	}
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

func MinimalEnvironment(overrides map[string]string, workspace string) []string {
	values := MinimalEnvironmentMap(overrides, workspace)
	environment := make([]string, 0, len(values))
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment
}

// MinimalEnvironmentMap builds an execution's environment from nothing rather
// than inheriting the runner's, so a credential set on the runner host cannot
// reach an agent that was never granted it. Everything an execution is allowed
// to see arrives through `overrides`, which the dispatcher fills from the task
// packet's resolved environment references.
//
// HOME here is a floor, not a decision. It defaults to the workspace so a
// process always has *some* writable home, and the dispatcher overrides it with
// a directory outside the checkout (see dispatch.executionHome) -- which is
// where a file-delivered credential is placed, and the reason a harness looking
// under ~ finds one. Anything an image baked into its own HOME is deliberately
// still not found: an agent inherits nothing it was not given.
func MinimalEnvironmentMap(overrides map[string]string, workspace string) map[string]string {
	environment := map[string]string{
		"HOME":   workspace,
		"PATH":   "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TMPDIR": filepath.Join(workspace, ".loop", "tmp"),
	}
	for key, value := range overrides {
		environment[key] = value
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

// silenceWatch bounds how long a process may run without writing to its output
// streams. Every write touches the watch; a ticker fires the expired channel
// once the process has been silent for the timeout. The check interval scales
// with the timeout so a short bound (tests) stays responsive and a long one
// does not spin.
type silenceWatch struct {
	mu      sync.Mutex
	timeout time.Duration
	last    time.Time
	fired   chan struct{}
	once    sync.Once
	stop    chan struct{}
}

func newSilenceWatch(timeout time.Duration) *silenceWatch {
	return &silenceWatch{timeout: timeout, last: time.Now(), fired: make(chan struct{}), stop: make(chan struct{})}
}

func (watch *silenceWatch) wrap(writer io.Writer) io.Writer {
	return silenceWriter{watch: watch, writer: writer}
}

func (watch *silenceWatch) touch() {
	watch.mu.Lock()
	watch.last = time.Now()
	watch.mu.Unlock()
}

func (watch *silenceWatch) run() {
	interval := watch.timeout / 10
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-watch.stop:
			return
		case <-ticker.C:
			watch.mu.Lock()
			idle := time.Since(watch.last) >= watch.timeout
			watch.mu.Unlock()
			if idle {
				watch.once.Do(func() { close(watch.fired) })
				return
			}
		}
	}
}

func (watch *silenceWatch) close() {
	close(watch.stop)
}

func (watch *silenceWatch) expired() <-chan struct{} {
	return watch.fired
}

type silenceWriter struct {
	watch  *silenceWatch
	writer io.Writer
}

func (writer silenceWriter) Write(contents []byte) (int, error) {
	writer.watch.touch()
	return writer.writer.Write(contents)
}
