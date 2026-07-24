package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	PipelineResults []pipeline.Result `json:"pipelineResults"`
}

func (dispatcher Dispatcher) Execute(ctx context.Context, lease control.Lease) (result Result, err error) {
	if dispatcher.Workspaces == nil || dispatcher.Backend == nil {
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
	request, err := prepareRequest(packet)
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
	environment, err := dispatcher.resolveEnvironment(ctx, packet.EnvironmentRefs)
	if err != nil {
		return Result{}, err
	}
	backendResult, executeErr := dispatcher.Backend.Execute(ctx, agents.Request{
		ExecutionID: packet.ExecutionID,
		Role:        agents.Role(packet.Role),
		Workspace:   workspace.Repository,
		Prompt:      promptFor(packet),
		ResultPath:  packet.ExpectedOutput,
		Timeout:     time.Duration(packet.TimeoutSeconds) * time.Second,
		Environment: environment,
	})
	result = Result{
		Status:          backendResult.Status,
		ExitCode:        backendResult.ExitCode,
		Summary:         backendResult.Summary,
		ChangedFiles:    backendResult.ChangedFiles,
		CommandsRun:     backendResult.CommandsRun,
		RemainingWork:   backendResult.RemainingWork,
		SessionID:       backendResult.SessionID,
		InitialRevision: initial.Revision,
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

func prepareRequest(packet taskpacket.Packet) (repository.PrepareRequest, error) {
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
	body := packet.Issue.Body
	if body == "" {
		body = "Implement the requested changes."
	}
	return fmt.Sprintf("# %s task\n\nObjective:\n%s\n\nIssue %s: %s\n\n%s\n\nAnalyze the repository, implement the changes, and verify they work.\n", packet.Role, packet.Objective, packet.Issue.ExternalID, packet.Issue.Title, body)
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
