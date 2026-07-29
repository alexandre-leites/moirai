package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

type WorkspaceManager interface {
	Prepare(context.Context, repository.PrepareRequest) (repository.Workspace, error)
	Cleanup(context.Context, string, string) error
	CleanupExisting(context.Context, string, string) error
}

type EnvironmentResolver interface {
	Resolve(context.Context, []taskpacket.EnvironmentRef) (map[string]string, error)
}

type RevisionInspector interface {
	Snapshot(context.Context, repository.Workspace) (repository.RevisionSummary, error)
}

type DeliveryManager interface {
	Commit(context.Context, repository.Workspace, string) (repository.CommitResult, error)
	Push(context.Context, repository.Workspace, string, map[string]string) (repository.PushResult, error)
}

type ProjectConcurrencyGuard struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func NewProjectConcurrencyGuard() *ProjectConcurrencyGuard {
	return &ProjectConcurrencyGuard{active: make(map[string]struct{})}
}

func (guard *ProjectConcurrencyGuard) Acquire(projectID string) (func(), error) {
	if guard == nil || projectID == "" {
		return nil, errors.New("project concurrency guard and project ID are required")
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	if _, active := guard.active[projectID]; active {
		return nil, fmt.Errorf("project %q already has an active runner execution", projectID)
	}
	guard.active[projectID] = struct{}{}
	return func() {
		guard.mu.Lock()
		delete(guard.active, projectID)
		guard.mu.Unlock()
	}, nil
}

type RetentionPolicy struct {
	KeepSucceeded bool
	KeepFailed    bool
	KeepAbandoned bool
}

type Dispatcher struct {
	Workspaces         WorkspaceManager
	Backend            agents.Backend
	Environment        EnvironmentResolver
	AllowedEnvironment []string
	MinimumFreeBytes   uint64
	DiskPath           string
	AvailableBytes     func(string) (uint64, error)
	RevisionInspector  RevisionInspector
	Pipeline           pipeline.Runner
	Retention          RetentionPolicy
	Projects           *ProjectConcurrencyGuard
	Delivery           DeliveryManager
	// EmitLog, when set, is called with each chunk of agent stdout/stderr
	// as it is produced, so it can be streamed to the orchestrator as log
	// events (see control.EventReporter.EmitLog).
	EmitLog func(jobID string, generation int64, message string) ([]int64, error)
}

type Result struct {
	Status          string            `json:"status"`
	ExitCode        int               `json:"exitCode"`
	Summary         string            `json:"summary"`
	ChangedFiles    []string          `json:"changedFiles"`
	CommandsRun     []string          `json:"commandsRun"`
	RemainingWork   []string          `json:"remainingWork"`
	SessionID       string            `json:"sessionId"`
	InitialRevision string            `json:"initialRevision"`
	FinalRevision   string            `json:"finalRevision"`
	Committed       bool              `json:"committed"`
	Branch          string            `json:"branch"`
	Pushed          bool              `json:"pushed"`
	PipelineResults []pipeline.Result `json:"pipelineResults"`
	Raw             map[string]any    `json:"raw,omitempty"`
}

func (dispatcher Dispatcher) Execute(ctx context.Context, lease control.Lease) (result Result, err error) {
	if dispatcher.Workspaces == nil || (lease.Packet.Role != taskpacket.RolePipeline && dispatcher.Backend == nil) {
		return Result{}, errors.New("workspace manager and agent backend are required")
	}
	if lease.JobID == "" || lease.Generation < 1 || lease.Packet.JobID != lease.JobID {
		return Result{}, errors.New("lease is invalid")
	}
	packet := lease.Packet
	if dispatcher.Projects != nil {
		release, err := dispatcher.Projects.Acquire(packet.Repository.ProjectID)
		if err != nil {
			return Result{}, err
		}
		defer release()
	}
	if dispatcher.MinimumFreeBytes > 0 {
		if dispatcher.AvailableBytes == nil {
			return Result{}, errors.New("disk availability probe is required")
		}
		available, diskErr := dispatcher.AvailableBytes(dispatcher.DiskPath)
		if diskErr != nil {
			return Result{}, fmt.Errorf("inspect workspace disk space: %w", diskErr)
		}
		if available < dispatcher.MinimumFreeBytes {
			return Result{}, fmt.Errorf("insufficient runner disk space: %d available, %d required", available, dispatcher.MinimumFreeBytes)
		}
	}
	// The task environment is resolved before the workspace exists: a clone or
	// fetch of a private repository needs the same credential the later push
	// does, and an unresolvable reference must fail the execution rather than
	// silently degrade to an unauthenticated Git operation.
	environment, err := dispatcher.resolveEnvironment(ctx, packet.EnvironmentRefs)
	if err != nil {
		return Result{}, err
	}
	request, err := prepareRequest(packet, environment)
	if err != nil {
		return Result{}, err
	}
	workspace, err := dispatcher.Workspaces.Prepare(ctx, request)
	if err != nil {
		return Result{}, fmt.Errorf("prepare workspace: %w", err)
	}
	defer func() {
		if dispatcher.shouldRetain(ctx, result, err) {
			return
		}
		cleanupErr := dispatcher.cleanup(context.Background(), packet)
		if cleanupErr != nil && err == nil {
			err = fmt.Errorf("cleanup workspace: %w", cleanupErr)
		}
	}()

	initial, err := dispatcher.snapshot(ctx, workspace)
	if err != nil {
		return Result{}, err
	}
	if err := writeArtifacts(workspace, packet); err != nil {
		return Result{}, err
	}
	if packet.Role == taskpacket.RolePipeline {
		pipelineResults, pipelineErr := dispatcher.runPipeline(ctx, workspace.Repository, packet.Pipeline)
		result = Result{Status: "completed", ExitCode: 0, InitialRevision: initial.Revision, PipelineResults: pipelineResults}
		if pipelineErr != nil {
			result.Status = "failed"
			result.Summary = pipelineErr.Error()
		}
		final, snapshotErr := dispatcher.snapshot(context.Background(), workspace)
		if snapshotErr != nil {
			return Result{}, snapshotErr
		}
		result.FinalRevision = final.Revision
		if artifactErr := writeTerminalResult(workspace, result); artifactErr != nil {
			return Result{}, artifactErr
		}
		if pipelineErr != nil {
			return result, fmt.Errorf("run pipeline: %w", pipelineErr)
		}
		return result, nil
	}

	output := dispatcher.logOutput(lease)
	backendResult, executeErr := dispatcher.Backend.Execute(ctx, agents.Request{
		ExecutionID: packet.ExecutionID,
		Role:        agents.Role(packet.Role),
		Workspace:   workspace.Repository,
		Prompt:      promptFor(packet),
		ResultPath:  packet.ExpectedOutput,
		Timeout:     time.Duration(packet.TimeoutSeconds) * time.Second,
		Environment: environment,
		Output:      output,
	})
	if forwarder, ok := output.(*logForwarder); ok {
		forwarder.Close()
	}
	result = Result{
		Status:          backendResult.Status,
		ExitCode:        backendResult.ExitCode,
		Summary:         backendResult.Summary,
		ChangedFiles:    backendResult.ChangedFiles,
		CommandsRun:     backendResult.CommandsRun,
		RemainingWork:   backendResult.RemainingWork,
		SessionID:       backendResult.SessionID,
		InitialRevision: initial.Revision,
		Raw:             backendResult.Raw,
	}
	if executeErr == nil && len(packet.Pipeline) > 0 {
		pipelineResults, pipelineErr := dispatcher.runPipeline(ctx, workspace.Repository, packet.Pipeline)
		result.PipelineResults = pipelineResults
		if pipelineErr != nil {
			executeErr = pipelineErr
			result.Status = "failed"
			if result.Summary == "" {
				result.Summary = pipelineErr.Error()
			}
		}
	}
	final, snapshotErr := dispatcher.snapshot(context.Background(), workspace)
	if snapshotErr != nil {
		return Result{}, snapshotErr
	}
	result.FinalRevision = final.Revision
	result.ChangedFiles = mergeChangedFiles(result.ChangedFiles, final.ChangedFiles)
	if executeErr == nil && packet.Constraints.MayModifyFiles {
		if deliverErr := dispatcher.deliver(ctx, workspace, packet, request.Branch, environment, &result); deliverErr != nil {
			executeErr = deliverErr
		}
	}
	if executeErr != nil {
		result.Status = "failed"
		if result.Summary == "" {
			result.Summary = executeErr.Error()
		}
	}
	if err := writeTerminalResult(workspace, result); err != nil {
		return Result{}, err
	}
	if executeErr != nil {
		return result, fmt.Errorf("execute agent: %w", executeErr)
	}
	return result, nil
}

func (dispatcher Dispatcher) Cancel(_ context.Context, lease control.Lease) error {
	if dispatcher.Backend == nil {
		return errors.New("agent backend is required")
	}
	if lease.JobID == "" || lease.Generation < 1 || lease.Packet.ExecutionID == "" {
		return errors.New("lease is invalid")
	}
	return dispatcher.Backend.Cancel(lease.Packet.ExecutionID)
}

func prepareRequest(packet taskpacket.Packet, environment map[string]string) (repository.PrepareRequest, error) {
	branch := packet.Repository.Branch
	if branch == "" {
		generated, err := repository.BranchName(packet.Issue.ExternalID, packet.ExecutionID)
		if err != nil {
			return repository.PrepareRequest{}, fmt.Errorf("generate repository branch: %w", err)
		}
		branch = generated
	}
	return repository.PrepareRequest{
		ProjectID:           packet.Repository.ProjectID,
		JobID:               packet.JobID,
		RepositoryMode:      repository.RepositoryMode(packet.Repository.Mode),
		RepositoryURL:       packet.Repository.URL,
		LocalRepositoryPath: packet.Repository.LocalPath,
		DefaultBranch:       packet.Repository.DefaultBranch,
		Branch:              branch,
		Environment:         environment,
	}, nil
}

func (dispatcher Dispatcher) runPipeline(ctx context.Context, workspace string, commands []taskpacket.PipelineCommand) ([]pipeline.Result, error) {
	runner := dispatcher.Pipeline
	if runner == nil {
		runner = pipeline.LocalRunner{}
	}
	pipelineCommands := make([]pipeline.Command, 0, len(commands))
	for _, command := range commands {
		pipelineCommands = append(pipelineCommands, pipeline.Command{Command: command.Command, Timeout: time.Duration(command.TimeoutSeconds) * time.Second})
	}
	return runner.Run(ctx, workspace, pipelineCommands)
}

func (dispatcher Dispatcher) snapshot(ctx context.Context, workspace repository.Workspace) (repository.RevisionSummary, error) {
	if dispatcher.RevisionInspector == nil {
		return repository.RevisionSummary{}, nil
	}
	summary, err := dispatcher.RevisionInspector.Snapshot(ctx, workspace)
	if err != nil {
		return repository.RevisionSummary{}, fmt.Errorf("capture repository revision: %w", err)
	}
	return summary, nil
}

// deliver commits the agent's changes and, when the packet permits pushing,
// pushes the resulting revision to the configured branch. It mutates result
// in place with the outcome so callers can report it in the terminal event.
func (dispatcher Dispatcher) deliver(ctx context.Context, workspace repository.Workspace, packet taskpacket.Packet, branch string, environment map[string]string, result *Result) error {
	if dispatcher.Delivery == nil {
		return errors.New("delivery manager is required for file-modifying executions")
	}
	commitResult, err := dispatcher.Delivery.Commit(ctx, workspace, commitMessageFor(packet))
	if err != nil {
		return fmt.Errorf("commit repository changes: %w", err)
	}
	result.Committed = commitResult.Committed
	if commitResult.Committed {
		result.FinalRevision = commitResult.Revision
	}
	if !commitResult.Committed || !packet.Constraints.MayPush {
		return nil
	}
	pushResult, err := dispatcher.Delivery.Push(ctx, workspace, branch, environment)
	if err != nil {
		return fmt.Errorf("push repository changes: %w", err)
	}
	result.Pushed = pushResult.Pushed
	result.Branch = pushResult.Branch
	return nil
}

func commitMessageFor(packet taskpacket.Packet) string {
	title := packet.Issue.Title
	if title == "" {
		title = packet.Objective
	}
	return fmt.Sprintf("%s: %s (%s)", packet.Role, title, packet.Issue.ExternalID)
}

func (dispatcher Dispatcher) logOutput(lease control.Lease) io.Writer {
	if dispatcher.EmitLog == nil {
		return nil
	}
	forwarder := &logForwarder{jobID: lease.JobID, generation: lease.Generation, emit: dispatcher.EmitLog, queue: make(chan string, 128), done: make(chan struct{})}
	go forwarder.run()
	return forwarder
}

type logForwarder struct {
	jobID      string
	generation int64
	emit       func(jobID string, generation int64, message string) ([]int64, error)
	queue      chan string
	done       chan struct{}
	closed     sync.Once
	dropped    atomic.Uint64
}

func (forwarder *logForwarder) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	select {
	case forwarder.queue <- string(data):
	default:
		forwarder.dropped.Add(1)
	}
	return len(data), nil
}

func (forwarder *logForwarder) Close() {
	forwarder.closed.Do(func() {
		close(forwarder.queue)
		<-forwarder.done
	})
}

func (forwarder *logForwarder) run() {
	defer close(forwarder.done)
	for message := range forwarder.queue {
		if _, err := forwarder.emit(forwarder.jobID, forwarder.generation, message); err != nil {
			forwarder.dropped.Add(1)
		}
	}
	if dropped := forwarder.dropped.Load(); dropped > 0 {
		slog.Warn("runner log events dropped", "job_id", forwarder.jobID, "lease_generation", forwarder.generation, "dropped", dropped)
	}
}

func mergeChangedFiles(primary, additional []string) []string {
	seen := make(map[string]struct{}, len(primary)+len(additional))
	merged := make([]string, 0, len(primary)+len(additional))
	for _, files := range [][]string{primary, additional} {
		for _, file := range files {
			if _, exists := seen[file]; file != "" && !exists {
				seen[file] = struct{}{}
				merged = append(merged, file)
			}
		}
	}
	return merged
}

func (dispatcher Dispatcher) shouldRetain(ctx context.Context, result Result, executeErr error) bool {
	if ctx.Err() != nil {
		return dispatcher.Retention.KeepAbandoned
	}
	if executeErr == nil && result.Status == "completed" {
		return dispatcher.Retention.KeepSucceeded
	}
	return dispatcher.Retention.KeepFailed
}

func (dispatcher Dispatcher) cleanup(ctx context.Context, packet taskpacket.Packet) error {
	if packet.Repository.Mode == string(repository.RepositoryModeExistingPath) {
		return dispatcher.Workspaces.CleanupExisting(ctx, packet.Repository.LocalPath, packet.JobID)
	}
	return dispatcher.Workspaces.Cleanup(ctx, packet.Repository.ProjectID, packet.JobID)
}

func (dispatcher Dispatcher) resolveEnvironment(ctx context.Context, references []taskpacket.EnvironmentRef) (map[string]string, error) {
	if len(references) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(dispatcher.AllowedEnvironment))
	for _, name := range dispatcher.AllowedEnvironment {
		allowed[name] = struct{}{}
	}
	for _, reference := range references {
		if _, ok := allowed[reference.Name]; !ok {
			return nil, fmt.Errorf("task environment %q is not allowed on this runner", reference.Name)
		}
	}
	if dispatcher.Environment == nil {
		return nil, errors.New("environment resolver is required for task secret references")
	}
	environment, err := dispatcher.Environment.Resolve(ctx, references)
	if err != nil {
		return nil, fmt.Errorf("resolve task environment: %w", err)
	}
	if len(environment) != len(references) {
		return nil, errors.New("resolved task environment is incomplete")
	}
	for _, reference := range references {
		if _, ok := environment[reference.Name]; !ok {
			return nil, fmt.Errorf("resolved task environment %q is missing", reference.Name)
		}
	}
	for name := range environment {
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("resolved task environment %q is not allowed", name)
		}
	}
	return environment, nil
}

func writeArtifacts(workspace repository.Workspace, packet taskpacket.Packet) error {
	if workspace.Root == "" || workspace.Loop == "" || workspace.Repository == "" {
		return errors.New("prepared workspace is invalid")
	}
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		return fmt.Errorf("create task artifact directory: %w", err)
	}
	packetContents, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task packet artifact: %w", err)
	}
	if err := writePrivateFile(filepath.Join(workspace.Loop, "task-packet.json"), append(packetContents, '\n')); err != nil {
		return err
	}
	return writePrivateFile(filepath.Join(workspace.Repository, packet.PromptPath), []byte(promptFor(packet)))
}

func promptFor(packet taskpacket.Packet) string {
	if packet.Role == taskpacket.RoleReviewer {
		return reviewerPromptFor(packet)
	}
	return executionPromptFor(packet)
}

func executionPromptFor(packet taskpacket.Packet) string {
	return strings.Join([]string{
		"# ROLE", string(packet.Role),
		"# IMMUTABLE OBJECTIVE", packet.Objective,
		"# ISSUE", fmt.Sprintf("%s: %s\n%s", packet.Issue.ExternalID, packet.Issue.Title, defaultText(packet.Issue.Body, "No additional issue body was supplied.")),
		"# ACCEPTANCE CRITERIA", listText(packet.AcceptanceCriteria, "No explicit acceptance criteria were supplied."),
		"# CURRENT PLAN", listText(packet.Plan, "No plan has been recorded yet."),
		"# CURRENT REPOSITORY COMMIT", defaultText(packet.CurrentCommit, "Unknown"),
		"# CURRENT DIFF SUMMARY", defaultText(packet.DiffSummary, "No diff summary is available."),
		"# FAILED CHECKS", listText(packet.FailedChecks, "No failed checks are recorded."),
		"# AI REVIEW FINDINGS", listText(packet.ReviewFindings, "No review findings are recorded."),
		"# PREVIOUS FAILURES", listText(packet.PreviousFailures, "No previous failures are recorded."),
		"# ALLOWED ACTIONS", allowedActions(packet),
		"# FORBIDDEN ACTIONS", "Do not merge, alter credentials, or claim success without writing the required result.",
		"# EXPECTED OUTPUT SCHEMA", expectedOutputSchema(packet.Role),
	}, "\n\n") + "\n"
}

func reviewerPromptFor(packet taskpacket.Packet) string {
	return strings.Join([]string{
		"# ROLE", "Independent reviewer. Evaluate the repository change; do not defend or continue a developer session.",
		"# IMMUTABLE OBJECTIVE", packet.Objective,
		"# ISSUE", fmt.Sprintf("%s: %s\n%s", packet.Issue.ExternalID, packet.Issue.Title, defaultText(packet.Issue.Body, "No additional issue body was supplied.")),
		"# ACCEPTANCE CRITERIA", listText(packet.AcceptanceCriteria, "No explicit acceptance criteria were supplied."),
		"# CURRENT REPOSITORY COMMIT", defaultText(packet.CurrentCommit, "Unknown"),
		"# CURRENT DIFF SUMMARY", defaultText(packet.DiffSummary, "Inspect the repository diff independently."),
		"# FAILED CHECKS", listText(packet.FailedChecks, "No failed checks are recorded."),
		"# AI REVIEW FINDINGS", listText(packet.ReviewFindings, "No prior review findings are recorded."),
		"# ALLOWED ACTIONS", "Inspect files, diffs, tests, and run read-only validation. Do not modify, push, or merge files.",
		"# FORBIDDEN ACTIONS", "Do not use developer conversation history, infer hidden reasoning, modify files, push, merge, or defend an implementation.",
		"# EXPECTED OUTPUT SCHEMA", expectedOutputSchema(packet.Role),
	}, "\n\n") + "\n"
}

func listText(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	return "- " + strings.Join(values, "\n- ")
}

func defaultText(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func allowedActions(packet taskpacket.Packet) string {
	actions := []string{"Inspect the repository and run relevant validation."}
	if packet.Constraints.MayModifyFiles {
		actions = append(actions, "Modify files needed to satisfy the objective.")
	}
	if packet.Constraints.MayPush {
		actions = append(actions, "Commit and push the prepared branch.")
	}
	return strings.Join(actions, " ")
}

func expectedOutputSchema(role taskpacket.Role) string {
	if role == taskpacket.RoleReviewer {
		return `Write .loop/result.json with protocolVersion, executionId, status, summary, verdict, acceptanceCriteria, and findings.`
	}
	return `Write .loop/result.json with protocolVersion, executionId, status, summary, changedFiles, commandsRun, remainingWork, and sessionId.`
}

func writeTerminalResult(workspace repository.Workspace, result Result) error {
	contents, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode terminal result: %w", err)
	}
	return writePrivateFile(filepath.Join(workspace.Loop, "terminal-result.json"), append(contents, '\n'))
}

func writePrivateFile(path string, contents []byte) error {
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		return fmt.Errorf("write task artifact: %w", err)
	}
	return nil
}
