package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/loop-engineering/runner/internal/execution"
)

type DockerCLIBackend struct {
	Image     string
	Arguments []string
	Executor  execution.DockerExecutor
}

func (backend DockerCLIBackend) Name() string {
	return "docker-cli"
}

func (backend DockerCLIBackend) HealthCheck(context.Context) error {
	if backend.Image == "" {
		return errors.New("Docker CLI backend image is required")
	}
	return nil
}

func (backend DockerCLIBackend) Execute(ctx context.Context, request Request) (Result, error) {
	if request.Prompt == "" || request.ExecutionID == "" {
		return Result{}, errors.New("Docker CLI backend prompt and execution ID are required")
	}
	if err := backend.HealthCheck(ctx); err != nil {
		return Result{}, err
	}
	resultPath, err := resultPathWithinWorkspace(request.Workspace, request.ResultPath)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o750); err != nil {
		return Result{}, fmt.Errorf("create result directory: %w", err)
	}
	stdout, stdoutLog, err := openAgentLog(filepath.Dir(resultPath), "docker-cli.stdout.log")
	if err != nil {
		return Result{}, err
	}
	defer stdout.Close()
	stderr, stderrLog, err := openAgentLog(filepath.Dir(resultPath), "docker-cli.stderr.log")
	if err != nil {
		return Result{}, err
	}
	defer stderr.Close()
	defer writeLogMetadata(filepath.Dir(resultPath), "docker-cli", stdoutLog, stderrLog)
	command := append(append([]string(nil), backend.Arguments...), request.Prompt)
	executor := backend.Executor
	executor.Image = backend.Image
	executionResult, err := executor.Execute(ctx, execution.Request{ExecutionID: request.ExecutionID, Workspace: request.Workspace, Command: command, Environment: request.Environment, Timeout: request.Timeout, OnStarted: func(pid int) {
		writeExecutionManifest(filepath.Dir(resultPath), "docker-cli", request.ExecutionID, pid)
	}}, streamedWriter(stdoutLog, request.Output), streamedWriter(stderrLog, request.Output))
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, fmt.Errorf("Docker CLI backend execution failed: %w", err)
	}
	document, err := readResultDocument(resultPath, request.ExecutionID)
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, err
	}
	return Result{Status: document.Status, ExitCode: executionResult.ExitCode, Summary: document.Summary, ChangedFiles: document.ChangedFiles, CommandsRun: document.CommandsRun, RemainingWork: document.RemainingWork, SessionID: document.SessionID}, nil
}

// Continue re-engages the agent with the continuation prompt. Each Docker
// invocation is a fresh container with no session to resume, so this is the
// fresh-run fallback the Backend contract allows; the continuation prompt still
// names the objective and the missing evidence.
func (backend DockerCLIBackend) Continue(ctx context.Context, request Request) (Result, error) {
	return backend.Execute(ctx, request)
}

func (backend DockerCLIBackend) Cancel(executionID string) error {
	return backend.Executor.Cancel(executionID)
}
