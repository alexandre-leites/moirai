package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultDockerStopTimeout = 10 * time.Second

type DockerExecutor struct {
	Binary      string
	Image       string
	CPULimit    string
	MemoryLimit string
	Network     string
	StopTimeout time.Duration
	Supervisor  *Supervisor
}

func (executor DockerExecutor) Name() string {
	return "docker"
}

func (executor DockerExecutor) Execute(parent context.Context, request Request, stdout io.Writer, stderr io.Writer) (Result, error) {
	if err := executor.validate(request); err != nil {
		return Result{}, err
	}

	workspace, err := filepath.Abs(filepath.Clean(request.Workspace))
	if err != nil {
		return Result{}, fmt.Errorf("resolve workspace: %w", err)
	}
	containerName := dockerContainerName(request.ExecutionID)
	environmentFile, err := writeEnvironmentFile(request.Environment)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(environmentFile)
	command := executor.runCommand(containerName, workspace, request, environmentFile)
	ctx, cancel := context.WithTimeout(parent, request.Timeout)
	defer cancel()
	done := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		select {
		case <-ctx.Done():
			select {
			case <-done:
				return
			default:
				_ = executor.stopWithTimeout(containerName)
			}
		case <-done:
		}
	}()

	result, executeErr := executor.supervisor().Execute(ctx, Request{
		ExecutionID: request.ExecutionID,
		Workspace:   workspace,
		Command:     command,
		Timeout:     request.Timeout,
		OnStarted:   request.OnStarted,
	}, stdout, stderr)
	close(done)
	<-monitorDone
	return result, executeErr
}

func (executor DockerExecutor) Cancel(executionID string) error {
	if executionID == "" {
		return errors.New("execution ID is required")
	}
	containerName := dockerContainerName(executionID)
	stopErr := executor.stopWithTimeout(containerName)
	cancelErr := executor.supervisor().Cancel(executionID)
	if cancelErr == nil {
		return nil
	}
	if stopErr == nil {
		return nil
	}
	return fmt.Errorf("cancel Docker execution: stop container: %v; stop client: %w", stopErr, cancelErr)
}

func (executor DockerExecutor) validate(request Request) error {
	if executor.Image == "" || strings.ContainsAny(executor.Image, " \t\r\n\x00") {
		return errors.New("Docker image is required and must not contain whitespace")
	}
	if request.ExecutionID == "" {
		return errors.New("execution ID is required")
	}
	if request.Workspace == "" {
		return errors.New("workspace is required")
	}
	if len(request.Command) == 0 || request.Command[0] == "" {
		return errors.New("command is required")
	}
	if request.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	for key, value := range request.Environment {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid environment variable %q", key)
		}
	}
	return nil
}

func (executor DockerExecutor) runCommand(containerName, workspace string, request Request, environmentFile string) []string {
	command := []string{executor.binary(), "run", "--rm", "--init", "--name", containerName, "--workdir", "/workspace", "--mount", "type=bind,src=" + workspace + ",dst=/workspace"}
	network := executor.Network
	if network == "" {
		network = "bridge"
	}
	command = append(command, "--network", network)
	if executor.CPULimit != "" {
		command = append(command, "--cpus", executor.CPULimit)
	}
	if executor.MemoryLimit != "" {
		command = append(command, "--memory", executor.MemoryLimit)
	}
	if environmentFile != "" {
		command = append(command, "--env-file", environmentFile)
	}
	if keyPath := request.Environment["GIT_SSH_KEY"]; filepath.IsAbs(keyPath) {
		command = append(command, "--mount", "type=bind,src="+keyPath+",dst="+keyPath+",readonly")
	}
	command = append(command, executor.Image)
	return append(command, request.Command...)
}

func writeEnvironmentFile(environment map[string]string) (string, error) {
	file, err := os.CreateTemp("", "loop-docker-env-*")
	if err != nil {
		return "", fmt.Errorf("create Docker environment file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("secure Docker environment file: %w", err)
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, environment[key]); err != nil {
			file.Close()
			os.Remove(path)
			return "", fmt.Errorf("write Docker environment file: %w", err)
		}
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close Docker environment file: %w", err)
	}
	return path, nil
}

func (executor DockerExecutor) stopWithTimeout(containerName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), executor.stopTimeout())
	defer cancel()
	return executor.stop(ctx, containerName)
}

func (executor DockerExecutor) stopTimeout() time.Duration {
	if executor.StopTimeout > 0 {
		return executor.StopTimeout
	}
	return defaultDockerStopTimeout
}

func (executor DockerExecutor) stop(ctx context.Context, containerName string) error {
	seconds := int(executor.stopTimeout().Seconds())
	if seconds < 1 {
		seconds = 1
	}
	command := exec.CommandContext(ctx, executor.binary(), "stop", "--time", strconv.Itoa(seconds), containerName)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop %s: %w: %s", containerName, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (executor DockerExecutor) binary() string {
	if executor.Binary == "" {
		return "docker"
	}
	return executor.Binary
}

func (executor DockerExecutor) supervisor() *Supervisor {
	if executor.Supervisor != nil {
		return executor.Supervisor
	}
	return defaultDockerSupervisor
}

var defaultDockerSupervisor = NewSupervisor()

func dockerContainerName(executionID string) string {
	digest := sha256.Sum256([]byte(executionID))
	return "loop-" + hex.EncodeToString(digest[:12])
}
