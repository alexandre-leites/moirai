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
	stdout, err := os.Create(filepath.Join(filepath.Dir(resultPath), "docker-cli.stdout.log"))
	if err != nil {
		return Result{}, fmt.Errorf("create Docker CLI stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(filepath.Dir(resultPath), "docker-cli.stderr.log"))
	if err != nil {
		return Result{}, fmt.Errorf("create Docker CLI stderr log: %w", err)
	}
	defer stderr.Close()
	stdoutLog := newBoundedLogWriter(stdout)
	stderrLog := newBoundedLogWriter(stderr)
	defer writeLogMetadata(filepath.Dir(resultPath), "docker-cli", stdoutLog, stderrLog)
	command := append(append([]string(nil), backend.Arguments...), request.Prompt)
	executor := backend.Executor
	executor.Image = backend.Image
	executionResult, err := executor.Execute(ctx, execution.Request{ExecutionID: request.ExecutionID, Workspace: request.Workspace, Command: command, Environment: request.Environment, Timeout: request.Timeout, OnStarted: func(pid int) {
		writeExecutionManifest(filepath.Dir(resultPath), "docker-cli", request.ExecutionID, pid)
	}}, stdoutLog, stderrLog)
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, fmt.Errorf("Docker CLI backend execution failed: %w", err)
	}
	document, err := readResultDocument(resultPath, request.ExecutionID)
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, err
	}
	return Result{Status: document.Status, ExitCode: executionResult.ExitCode, Summary: document.Summary, ChangedFiles: document.ChangedFiles, CommandsRun: document.CommandsRun, RemainingWork: document.RemainingWork, SessionID: document.SessionID}, nil
}

func (backend DockerCLIBackend) Cancel(executionID string) error {
	return backend.Executor.Cancel(executionID)
}
