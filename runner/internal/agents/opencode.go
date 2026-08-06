package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	JobID           string
	LeaseGeneration int64
	ExecutionID     string
	Role            Role
	Workspace       string
	Prompt          string
	ResultPath      string
	Timeout         time.Duration
	Environment     map[string]string
	// Silence bounds how long the agent process may run without producing any
	// output. Zero disables the bound. A silent agent is wedged rather than
	// working, so the runner terminates it and the goal gate's continuation
	// loop re-engages it instead of waiting out the whole timeout.
	Silence time.Duration
	// Output, when set, receives a live copy of the agent's stdout and
	// stderr as it is produced, in addition to the on-disk log files.
	Output io.Writer
	// SessionID names the agent session a continuation resumes. It carries the
	// `sessionId` the previous run's result document reported and is read only
	// by Continue; Execute always starts a fresh session.
	SessionID string
}

type Result struct {
	Status        string
	ExitCode      int
	Summary       string
	ChangedFiles  []string
	CommandsRun   []string
	RemainingWork []string
	SessionID     string
	Raw           map[string]any
}

type Backend interface {
	Name() string
	HealthCheck(context.Context) error
	Execute(context.Context, Request) (Result, error)
	// Continue re-engages the agent for the same execution after the runner's
	// goal gate found the objective unmet, resuming request.SessionID where the
	// backend keeps sessions. Backends without a session concept — and a
	// session that was never captured — fall back to a fresh run carrying the
	// continuation prompt, which still names the missing evidence, so the
	// contract holds for every backend rather than only for opencode.
	//
	// It is a distinct method rather than a flag on Execute so that a backend
	// which cannot resume has to say so in code, instead of silently ignoring a
	// session ID and reporting a fresh run as a continuation.
	Continue(context.Context, Request) (Result, error)
	Cancel(string) error
}

// ErrNoResultEvidence reports an agent process that ended successfully without
// leaving a valid result document. Deterministic gates decide completion, so an
// execution with no evidence is a failure rather than a silent success.
//
// It is deliberately distinct from a process failure: a failing process reports
// the executor error instead, so the two produce different failure fingerprints
// downstream and the orchestrator can tell "the agent crashed" apart from "the
// agent claimed nothing". Failure text stays free of absolute workspace paths
// to keep those fingerprints stable across executions.
var ErrNoResultEvidence = errors.New("agent exited 0 without a valid result document")

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
	return backend.run(parent, request, false)
}

// Continue resumes the opencode session the previous run's result document
// reported, so the agent keeps its own reasoning context across a goal-gate
// continuation instead of re-deriving it from the prompt. An execution whose
// agent never reported a session ID — most often because it wrote no result
// document at all — falls back to a fresh run, which is still driven by a
// continuation prompt naming the missing evidence.
func (backend OpenCodeBackend) Continue(parent context.Context, request Request) (Result, error) {
	return backend.run(parent, request, true)
}

func (backend OpenCodeBackend) run(parent context.Context, request Request, resume bool) (Result, error) {
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
	stdout, stdoutLog, err := openAgentLog(filepath.Dir(resultPath), "opencode.stdout.log")
	if err != nil {
		return Result{}, err
	}
	defer stdout.Close()
	stderr, stderrLog, err := openAgentLog(filepath.Dir(resultPath), "opencode.stderr.log")
	if err != nil {
		return Result{}, err
	}
	defer stderr.Close()

	defer writeLogMetadata(filepath.Dir(resultPath), "opencode", stdoutLog, stderrLog)
	defer os.Remove(filepath.Join(filepath.Dir(resultPath), "execution-manifest.json"))
	supervisor := backend.supervisor()
	executionResult, err := supervisor.Execute(parent, execution.Request{
		ExecutionID: request.ExecutionID,
		Workspace:   request.Workspace,
		Command:     backend.command(request, resume),
		Environment: request.Environment,
		Timeout:     request.Timeout,
		Silence:     request.Silence,
		OnStarted: func(pid int) {
			writeExecutionManifest(filepath.Dir(resultPath), "opencode", request.JobID, request.LeaseGeneration, request.ExecutionID, pid)
		},
	}, streamedWriter(stdoutLog, request.Output), streamedWriter(stderrLog, request.Output))

	raw := readRawResultDocument(resultPath)

	document, docErr := readResultDocument(resultPath, request.ExecutionID, request.Role)
	if docErr == nil {
		return Result{
			Status:        document.Status,
			ExitCode:      executionResult.ExitCode,
			Summary:       document.Summary,
			ChangedFiles:  document.ChangedFiles,
			CommandsRun:   document.CommandsRun,
			RemainingWork: document.RemainingWork,
			SessionID:     document.SessionID,
			Raw:           raw,
		}, err
	}

	// Without a valid result document there is no evidence of what the agent
	// did, so the execution can never be reported as completed. A failing
	// process is the root cause when the command itself failed; otherwise the
	// agent ended its own loop early, refused, or crashed its reasoning while
	// exiting cleanly.
	failure := err
	summary := ""
	if failure == nil {
		failure = fmt.Errorf("%w (%s): %w", ErrNoResultEvidence, workspaceRelativePath(request.Workspace, resultPath), docErr)
		summary = failure.Error()
	}
	return Result{
		Status:   "failed",
		ExitCode: executionResult.ExitCode,
		Summary:  summary,
		Raw:      raw,
	}, failure
}

func (backend OpenCodeBackend) Cancel(executionID string) error {
	return backend.supervisor().Cancel(executionID)
}

// command builds the opencode invocation. A continuation adds `--session
// <id>`, opencode's own session-resume selector, immediately before the prompt;
// everything else — the configured arguments and the workspace directory — is
// identical to a first run, so a resumed run cannot drift from the contract the
// first one ran under.
func (backend OpenCodeBackend) command(request Request, resume bool) []string {
	command := append([]string{backend.binary(), "run"}, backend.Arguments...)
	command = append(command, "--dir", request.Workspace)
	if resume && request.SessionID != "" {
		command = append(command, "--session", request.SessionID)
	}
	return append(command, "Read .loop/prompt.md and follow its instructions.")
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

// streamedWriter tees agent output into the on-disk bounded log writer and,
// when configured, a live sink so operators can observe execution progress
// without shell access to the runner.
func streamedWriter(log *boundedLogWriter, output io.Writer) io.Writer {
	if output == nil {
		return log
	}
	return io.MultiWriter(log, output)
}

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

// workspaceRelativePath renders a workspace file location for failure messages.
// The relative form names the missing evidence without embedding the volatile
// absolute workspace path, which would otherwise make every failure fingerprint
// unique and defeat non-progress detection.
func workspaceRelativePath(workspace, path string) string {
	root, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return filepath.Base(path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return relative
}

// readRawResultDocument returns the agent's .loop/result.json contents as a
// generic object so role-specific fields (e.g. a reviewer's verdict) reach
// the orchestrator even though resultDocument only decodes the fields common
// to every role. It is best-effort: any read or parse failure yields nil.
func readRawResultDocument(path string) map[string]any {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		return nil
	}
	return document
}

func readResultDocument(path, executionID string, role Role) (resultDocument, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Reported without the path so repeated no-evidence failures share
			// one stable failure fingerprint.
			return resultDocument{}, errors.New("agent result was not written")
		}
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
	case "planned":
		// A planner's terminal status is "planned" (#351): the plan it wrote is
		// its deliverable, not a code change. Normalize it to "completed" so the
		// runner reports the planner execution as a success and the orchestrator
		// folds the plan into the developer packet (phase_dispatch.go's
		// plannerCompleted branch). Any other role writing "planned" is a
		// malformed result, not a planner.
		if role != RolePlanner {
			return resultDocument{}, fmt.Errorf("agent result has invalid status %q", document.Status)
		}
		document.Status = "completed"
		return document, nil
	default:
		return resultDocument{}, fmt.Errorf("agent result has invalid status %q", document.Status)
	}
}
