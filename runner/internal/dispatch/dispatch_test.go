package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

type workspaceManager struct {
	workspace  repository.Workspace
	prepareErr error
	prepared   repository.PrepareRequest
	cleaned    bool
	artifacts  map[string]string
}

func (manager *workspaceManager) Prepare(_ context.Context, request repository.PrepareRequest) (repository.Workspace, error) {
	manager.prepared = request
	if manager.prepareErr != nil {
		return repository.Workspace{}, manager.prepareErr
	}
	for _, path := range []string{manager.workspace.Root, manager.workspace.Repository, manager.workspace.Loop} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return repository.Workspace{}, err
		}
	}
	return manager.workspace, nil
}

func (manager *workspaceManager) Cleanup(_ context.Context, _, _ string) error {
	return manager.recordArtifacts()
}

func (manager *workspaceManager) CleanupExisting(_ context.Context, _, _ string) error {
	return manager.recordArtifacts()
}

func (manager *workspaceManager) recordArtifacts() error {
	manager.cleaned = true
	manager.artifacts = make(map[string]string)
	for _, name := range []string{"task-packet.json", "prompt.md", "terminal-result.json"} {
		contents, err := os.ReadFile(filepath.Join(manager.workspace.Loop, name))
		if err != nil {
			return err
		}
		manager.artifacts[name] = string(contents)
	}
	return nil
}

type revisionInspector struct {
	summaries []repository.RevisionSummary
	calls     int
}

func (inspector *revisionInspector) Snapshot(context.Context, repository.Workspace) (repository.RevisionSummary, error) {
	if inspector.calls >= len(inspector.summaries) {
		return repository.RevisionSummary{}, errors.New("unexpected snapshot")
	}
	summary := inspector.summaries[inspector.calls]
	inspector.calls++
	return summary, nil
}

type backend struct {
	request agents.Request
	result  agents.Result
	err     error
}

func (backend *backend) Name() string                      { return "fake" }
func (backend *backend) HealthCheck(context.Context) error { return nil }
func (backend *backend) Cancel(string) error               { return nil }
func (backend *backend) Execute(_ context.Context, request agents.Request) (agents.Result, error) {
	backend.request = request
	return backend.result, backend.err
}

func TestDispatcherReturnsPreparationFailureWithoutExecuting(t *testing.T) {
	manager := &workspaceManager{prepareErr: errors.New("repository unavailable")}
	agent := &backend{}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	_, err := dispatcher.Execute(context.Background(), validLease())
	if err == nil || manager.cleaned {
		t.Fatalf("Execute() = %v, cleaned = %v", err, manager.cleaned)
	}
	if agent.request.ExecutionID != "" {
		t.Fatalf("backend received request %#v after preparation failure", agent.request)
	}
}

func TestDispatcherPreparesPersistsExecutesAndCleansUp(t *testing.T) {
	root := t.TempDir()
	manager := &workspaceManager{workspace: repository.Workspace{
		Root:       root,
		Repository: filepath.Join(root, "repository"),
		Loop:       filepath.Join(root, "repository", ".loop"),
	}}
	agent := &backend{result: agents.Result{Status: "completed", Summary: "implemented", ExitCode: 0, ChangedFiles: []string{"main.go"}, SessionID: "session-1"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	result, err := dispatcher.Execute(context.Background(), validLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || result.SessionID != "session-1" || !manager.cleaned {
		t.Fatalf("result = %#v, cleaned = %v", result, manager.cleaned)
	}
	if manager.prepared.ProjectID != "project-1" || manager.prepared.Branch != "agent/issue-7/run-1" {
		t.Fatalf("prepare request = %#v", manager.prepared)
	}
	if agent.request.Workspace != manager.workspace.Repository || agent.request.ResultPath != ".loop/result.json" || agent.request.Timeout != 10*time.Minute {
		t.Fatalf("agent request = %#v", agent.request)
	}
	if agent.request.Role != agents.RoleDeveloper || agent.request.Prompt == "" {
		t.Fatalf("agent request = %#v", agent.request)
	}
	for _, name := range []string{"task-packet.json", "prompt.md", "terminal-result.json"} {
		if manager.artifacts[name] == "" {
			t.Fatalf("%s was not persisted: %#v", name, manager.artifacts)
		}
	}
}

func TestDispatcherCapturesRepositoryRevisionAndChanges(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	inspector := &revisionInspector{summaries: []repository.RevisionSummary{
		{Revision: "initial", ChangedFiles: []string{"untracked.txt"}},
		{Revision: "final", ChangedFiles: []string{"untracked.txt", "updated.go"}},
	}}
	result, err := (Dispatcher{
		Workspaces:        manager,
		Backend:           &backend{result: agents.Result{Status: "completed", ChangedFiles: []string{"backend.go", "updated.go"}}},
		RevisionInspector: inspector,
	}).Execute(context.Background(), validLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.InitialRevision != "initial" || result.FinalRevision != "final" || inspector.calls != 2 {
		t.Fatalf("result = %#v, inspector calls = %d", result, inspector.calls)
	}
	if got, want := strings.Join(result.ChangedFiles, ","), "backend.go,updated.go,untracked.txt"; got != want {
		t.Fatalf("changed files = %q, want %q", got, want)
	}
}

func TestDispatcherRecordsPipelineFailure(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	lease := validLease()
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "false", TimeoutSeconds: 1}}
	result, err := (Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}}).Execute(context.Background(), lease)
	if err == nil || result.Status != "failed" || len(result.PipelineResults) != 1 || result.PipelineResults[0].ExitCode != 1 {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
}

func TestDispatcherGeneratesBranchFromIssueAndExecutionWhenOmitted(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	lease := validLease()
	lease.Packet.Repository.Branch = ""
	_, err := (Dispatcher{Workspaces: manager, Backend: &backend{}}).Execute(context.Background(), lease)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	branch, err := repository.BranchName(lease.Packet.Issue.ExternalID, lease.Packet.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if manager.prepared.Branch != branch {
		t.Fatalf("prepared branch = %q, want %q", manager.prepared.Branch, branch)
	}
}

func TestDispatcherRetainsWorkspacesByTerminalOutcome(t *testing.T) {
	tests := []struct {
		name      string
		context   context.Context
		backend   *backend
		retention RetentionPolicy
		retained  bool
	}{
		{name: "succeeded", context: context.Background(), backend: &backend{result: agents.Result{Status: "completed"}}, retention: RetentionPolicy{KeepSucceeded: true}, retained: true},
		{name: "failed", context: context.Background(), backend: &backend{err: errors.New("failed")}, retention: RetentionPolicy{KeepFailed: true}, retained: true},
		{name: "abandoned", context: canceledContext(), backend: &backend{err: context.Canceled}, retention: RetentionPolicy{KeepAbandoned: true}, retained: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &workspaceManager{workspace: testWorkspace(t)}
			_, _ = (Dispatcher{Workspaces: manager, Backend: test.backend, Retention: test.retention}).Execute(test.context, validLease())
			if manager.cleaned == test.retained {
				t.Fatalf("cleaned = %v, retained = %v", manager.cleaned, test.retained)
			}
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestDispatcherRejectsLowDiskBeforeWorkspacePreparation(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          &backend{},
		MinimumFreeBytes: 1024,
		DiskPath:         t.TempDir(),
		AvailableBytes:   func(string) (uint64, error) { return 512, nil },
	}
	_, err := dispatcher.Execute(context.Background(), validLease())
	if err == nil || manager.cleaned || manager.artifacts != nil {
		t.Fatalf("Execute() = %v, manager = %#v", err, manager)
	}
}

func TestDispatcherPersistsFailedTerminalResult(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{err: errors.New("agent exited")}

	result, err := (Dispatcher{Workspaces: manager, Backend: agent}).Execute(context.Background(), validLease())
	if err == nil || result.Status != "failed" || !manager.cleaned {
		t.Fatalf("Execute() = (%#v, %v), cleaned = %v", result, err, manager.cleaned)
	}
	if manager.artifacts["terminal-result.json"] == "" {
		t.Fatalf("terminal result was not persisted: %#v", manager.artifacts)
	}
}

type environmentResolver struct {
	values map[string]string
}

func (resolver environmentResolver) Resolve(context.Context, []taskpacket.EnvironmentRef) (map[string]string, error) {
	return resolver.values, nil
}

func TestDispatcherAllowsOnlyConfiguredTaskEnvironment(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{result: agents.Result{Status: "completed"}}
	lease := validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "TOKEN", SecretRef: "secret/token"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent, Environment: environmentResolver{values: map[string]string{"TOKEN": "resolved"}}, AllowedEnvironment: []string{"TOKEN"}}
	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if agent.request.Environment["TOKEN"] != "resolved" {
		t.Fatalf("agent environment = %#v", agent.request.Environment)
	}
	lease = validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "TOKEN", SecretRef: "secret/token"}}
	manager = &workspaceManager{workspace: testWorkspace(t)}
	agent = &backend{}
	_, err := (Dispatcher{Workspaces: manager, Backend: agent, Environment: environmentResolver{values: map[string]string{"TOKEN": "resolved"}}}).Execute(context.Background(), lease)
	if err == nil || agent.request.ExecutionID != "" {
		t.Fatalf("unallowed task environment error = %v, request = %#v", err, agent.request)
	}
}

func TestDispatcherFailsClosedWithoutEnvironmentResolver(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{}
	lease := validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "TOKEN", SecretRef: "secret/token"}}

	_, err := (Dispatcher{Workspaces: manager, Backend: agent}).Execute(context.Background(), lease)
	if err == nil || !manager.cleaned || agent.request.ExecutionID != "" {
		t.Fatalf("Execute() = %v, cleaned = %v, request = %#v", err, manager.cleaned, agent.request)
	}
}

func TestProjectConcurrencyGuardRejectsDuplicateProjectUntilReleased(t *testing.T) {
	guard := NewProjectConcurrencyGuard()
	release, err := guard.Acquire("project-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Acquire("project-1"); err == nil {
		t.Fatal("Acquire() accepted a duplicate project")
	}
	release()
	secondRelease, err := guard.Acquire("project-1")
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}

func validLease() control.Lease {
	return control.Lease{
		JobID: "job-1", Generation: 1,
		Packet: taskpacket.Packet{
			JobID: "job-1", ExecutionID: "execution-1", Role: taskpacket.RoleDeveloper,
			Objective: "Implement the task", Issue: taskpacket.Issue{ExternalID: "7", Title: "A task", Body: "Task body"},
			Repository: taskpacket.Repository{ProjectID: "project-1", Mode: "managed_clone", URL: "https://example.test/owner/repo.git", DefaultBranch: "main", Branch: "agent/issue-7/run-1"},
			PromptPath: ".loop/prompt.md", ExpectedOutput: ".loop/result.json", TimeoutSeconds: 600,
		},
	}
}

func testWorkspace(t *testing.T) repository.Workspace {
	t.Helper()
	root := t.TempDir()
	return repository.Workspace{Root: root, Repository: filepath.Join(root, "repository"), Loop: filepath.Join(root, "repository", ".loop")}
}
