package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/loop-engineering/runner/internal/execution"
)

type Role string

const (
	RolePlanner   Role = "planner"
	RoleDeveloper Role = "developer"
	RoleReviewer  Role = "reviewer"
	RoleRepairer  Role = "repairer"
)

type Request struct {
	ExecutionID string
	Role        Role
	Workspace   string
	Prompt      string
	ResultPath  string
	Timeout     time.Duration
	Environment map[string]string
}

type Result struct {
	Status        string
	ExitCode      int
	Summary       string
	ChangedFiles  []string
	CommandsRun   []string
	RemainingWork []string
	SessionID     string
}

type Backend interface {
	Name() string
	HealthCheck(context.Context) error
	Execute(context.Context, Request) (Result, error)
	Cancel(string) error
}

type OpenCodeBackend struct {
	Binary     string
	Arguments  []string
	Supervisor *execution.Supervisor
}

type resultDocument struct {
	ProtocolVersion string   `json:"protocolVersion"`
	ExecutionID     string   `json:"executionId"`
	Status          string   `json:"status"`
	Summary         string   `json:"summary"`
	ChangedFiles    []string `json:"changedFiles"`
	CommandsRun     []string `json:"commandsRun"`
	RemainingWork   []string `json:"remainingWork"`
	SessionID       string   `json:"sessionId"`
}

func (backend OpenCodeBackend) Name() string {
	return "opencode"
}

func (backend OpenCodeBackend) HealthCheck(context.Context) error {
	_, err := exec.LookPath(backend.binary())
	if err != nil {
		return fmt.Errorf("opencode executable unavailable: %w", err)
	}
	return nil
}

func (backend OpenCodeBackend) Execute(parent context.Context, request Request) (Result, error) {
	if request.Prompt == "" {
		return Result{}, errors.New("prompt is required")
	}
	if request.ExecutionID == "" {
		return Result{}, errors.New("execution ID is required")
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
	stdout, err := os.Create(filepath.Join(filepath.Dir(resultPath), "opencode.stdout.log"))
	if err != nil {
		return Result{}, fmt.Errorf("create stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(filepath.Dir(resultPath), "opencode.stderr.log"))
	if err != nil {
		return Result{}, fmt.Errorf("create stderr log: %w", err)
	}
	defer stderr.Close()

	stdoutLog := newBoundedLogWriter(stdout)
	stderrLog := newBoundedLogWriter(stderr)
	defer writeLogMetadata(filepath.Dir(resultPath), "opencode", stdoutLog, stderrLog)
	supervisor := backend.supervisor()
	executionResult, err := supervisor.Execute(parent, execution.Request{
		ExecutionID: request.ExecutionID,
		Workspace:   request.Workspace,
		Command:     append(append(append([]string{backend.binary(), "run"}, backend.Arguments...), "--dir", request.Workspace), request.Prompt),
		Environment: request.Environment,
		Timeout:     request.Timeout,
		OnStarted: func(pid int) {
			writeExecutionManifest(filepath.Dir(resultPath), "opencode", request.ExecutionID, pid)
		},
	}, stdoutLog, stderrLog)

	document, docErr := readResultDocument(resultPath, request.ExecutionID)
	if docErr == nil {
		return Result{
			Status:        document.Status,
			ExitCode:      executionResult.ExitCode,
			Summary:       document.Summary,
			ChangedFiles:  document.ChangedFiles,
			CommandsRun:   document.CommandsRun,
			RemainingWork: document.RemainingWork,
			SessionID:     document.SessionID,
		}, err
	}

	status := "completed"
	if err != nil {
		status = "failed"
	}
	return Result{
		Status:   status,
		ExitCode: executionResult.ExitCode,
	}, err
}

func (backend OpenCodeBackend) Cancel(executionID string) error {
	return backend.supervisor().Cancel(executionID)
}

func (backend OpenCodeBackend) binary() string {
	if backend.Binary == "" {
		return "opencode"
	}
	return backend.Binary
}

func (backend OpenCodeBackend) supervisor() *execution.Supervisor {
	if backend.Supervisor != nil {
		return backend.Supervisor
	}
	return defaultSupervisor
}

var defaultSupervisor = execution.NewSupervisor()

func resultPathWithinWorkspace(workspace, path string) (string, error) {
	if workspace == "" {
		return "", errors.New("workspace is required")
	}
	root, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if path == "" {
		path = filepath.Join(".loop", "result.json")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve result path: %w", err)
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("result path must be inside the workspace")
	}
	return candidate, nil
}

func readResultDocument(path, executionID string) (resultDocument, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return resultDocument{}, fmt.Errorf("read agent result: %w", err)
	}
	var document resultDocument
	if err := json.Unmarshal(contents, &document); err != nil {
		return resultDocument{}, fmt.Errorf("parse agent result: %w", err)
	}
	if document.ProtocolVersion != "1.0" {
		return resultDocument{}, errors.New("agent result protocolVersion must be 1.0")
	}
	if document.ExecutionID != executionID {
		return resultDocument{}, errors.New("agent result executionId does not match")
	}
	if document.Summary == "" {
		return resultDocument{}, errors.New("agent result summary is required")
	}
	switch document.Status {
	case "completed", "blocked", "failed":
		return document, nil
	default:
		return resultDocument{}, fmt.Errorf("agent result has invalid status %q", document.Status)
	}
}
