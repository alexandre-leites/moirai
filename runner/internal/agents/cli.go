package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/loop-engineering/runner/internal/execution"
)

type CLIBackend struct {
	NameValue  string
	Binary     string
	Arguments  []string
	Supervisor *execution.Supervisor
}

func (backend CLIBackend) Name() string {
	return backend.NameValue
}

func (backend CLIBackend) HealthCheck(context.Context) error {
	if backend.NameValue == "" || backend.Binary == "" {
		return errors.New("CLI backend name and binary are required")
	}
	if _, err := exec.LookPath(backend.Binary); err != nil {
		return fmt.Errorf("CLI backend executable unavailable: %w", err)
	}
	return nil
}

func (backend CLIBackend) Execute(parent context.Context, request Request) (Result, error) {
	if request.Prompt == "" || request.ExecutionID == "" {
		return Result{}, errors.New("CLI backend prompt and execution ID are required")
	}
	if err := backend.HealthCheck(parent); err != nil {
		return Result{}, err
	}
	resultPath, err := resultPathWithinWorkspace(request.Workspace, request.ResultPath)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o750); err != nil {
		return Result{}, fmt.Errorf("create result directory: %w", err)
	}
	stdout, stdoutLog, err := openAgentLog(filepath.Dir(resultPath), backend.NameValue+".stdout.log")
	if err != nil {
		return Result{}, err
	}
	defer stdout.Close()
	stderr, stderrLog, err := openAgentLog(filepath.Dir(resultPath), backend.NameValue+".stderr.log")
	if err != nil {
		return Result{}, err
	}
	defer stderr.Close()
	command := append(append([]string(nil), backend.Arguments...), request.Prompt)
	defer writeLogMetadata(filepath.Dir(resultPath), backend.NameValue, stdoutLog, stderrLog)
	defer os.Remove(filepath.Join(filepath.Dir(resultPath), "execution-manifest.json"))
	executionResult, err := backend.supervisor().Execute(parent, execution.Request{
		ExecutionID: request.ExecutionID,
		Workspace:   request.Workspace,
		Command:     append([]string{backend.Binary}, command...),
		Environment: request.Environment,
		Timeout:     request.Timeout,
		OnStarted: func(pid int) {
			writeExecutionManifest(filepath.Dir(resultPath), backend.NameValue, request.JobID, request.LeaseGeneration, request.ExecutionID, pid)
		},
	}, streamedWriter(stdoutLog, request.Output), streamedWriter(stderrLog, request.Output))
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, fmt.Errorf("CLI backend execution failed: %w", err)
	}
	document, err := readResultDocument(resultPath, request.ExecutionID, request.Role)
	if err != nil {
		return Result{ExitCode: executionResult.ExitCode}, err
	}
	return Result{Status: document.Status, ExitCode: executionResult.ExitCode, Summary: document.Summary, ChangedFiles: document.ChangedFiles, CommandsRun: document.CommandsRun, RemainingWork: document.RemainingWork, SessionID: document.SessionID}, nil
}

// Continue re-engages the agent with the continuation prompt. A generic CLI
// backend exposes no session-resume selector — the command line is whatever the
// operator configured — so this is the fresh-run fallback the Backend contract
// allows. The continuation still carries the objective and the missing evidence
// in its prompt, so the agent is re-engaged toward the goal; only the previous
// run's reasoning context is lost.
func (backend CLIBackend) Continue(ctx context.Context, request Request) (Result, error) {
	return backend.Execute(ctx, request)
}

func (backend CLIBackend) Cancel(executionID string) error {
	return backend.supervisor().Cancel(executionID)
}

func (backend CLIBackend) supervisor() *execution.Supervisor {
	if backend.Supervisor != nil {
		return backend.Supervisor
	}
	return defaultSupervisor
}
