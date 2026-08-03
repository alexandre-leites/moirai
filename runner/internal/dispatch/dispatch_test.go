package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

type workspaceManager struct {
	workspace  repository.Workspace
	prepareErr error
	releaseErr error
	prepared   repository.PrepareRequest
	cleaned    bool
	released   bool
	artifacts  map[string]string
}

func (manager *workspaceManager) ReleaseBranch(context.Context, repository.Workspace) error {
	if manager.releaseErr != nil {
		return manager.releaseErr
	}
	manager.released = true
	return nil
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

type pipelineRunner struct {
	results     []pipeline.Result
	err         error
	environment map[string]string
}

func (runner *pipelineRunner) Run(_ context.Context, _ string, environment map[string]string, _ []pipeline.Command) ([]pipeline.Result, error) {
	runner.environment = environment
	return runner.results, runner.err
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
	// onExecute observes the request while the agent is "running", for
	// assertions on state the dispatcher tears down on the way out.
	onExecute func(agents.Request)
}

func (backend *backend) Name() string                      { return "fake" }
func (backend *backend) HealthCheck(context.Context) error { return nil }
func (backend *backend) Cancel(string) error               { return nil }
func (backend *backend) Execute(_ context.Context, request agents.Request) (agents.Result, error) {
	backend.request = request
	if backend.onExecute != nil {
		backend.onExecute(request)
	}
	return backend.result, backend.err
}
func (backend *backend) Continue(ctx context.Context, request agents.Request) (agents.Result, error) {
	return backend.Execute(ctx, request)
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

func TestReviewerPromptUsesIndependentContextWithoutDeveloperPlan(t *testing.T) {
	packet := validLease().Packet
	packet.Role = taskpacket.RoleReviewer
	packet.AcceptanceCriteria = []string{"Review the implementation"}
	packet.Plan = []string{"developer private reasoning"}
	packet.DiffSummary = "main.go changed"
	prompt := promptFor(packet)
	if !strings.Contains(prompt, "# ROLE") || !strings.Contains(prompt, "# ACCEPTANCE CRITERIA") || !strings.Contains(prompt, "# EXPECTED OUTPUT SCHEMA") {
		t.Fatalf("prompt is missing required sections: %q", prompt)
	}
	if strings.Contains(prompt, "CURRENT PLAN") || strings.Contains(prompt, "developer private reasoning") || !strings.Contains(prompt, "do not defend") {
		t.Fatalf("reviewer prompt leaked developer context: %q", prompt)
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
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "false", TimeoutSeconds: 1, Required: true}}
	result, err := (Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}}).Execute(context.Background(), lease)
	if err == nil || result.Status != "failed" || len(result.PipelineResults) != 1 || result.PipelineResults[0].ExitCode != 1 {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
}

// TestDispatcherDoesNotGateOnANonRequiredPipelineFailure: web/src/projects.tsx
// promises operators "No required command blocks completion" for a pipeline
// step whose Required box is unchecked. A failing non-required command must
// still run (and be recorded), but must not turn a completed agent result
// into a failed one.
func TestDispatcherDoesNotGateOnANonRequiredPipelineFailure(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	lease := validLease()
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "false", TimeoutSeconds: 1, Required: false}}
	result, err := (Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}}).Execute(context.Background(), lease)
	if err != nil || result.Status != "completed" || len(result.PipelineResults) != 1 || result.PipelineResults[0].ExitCode != 1 {
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
			retention := test.retention
			retention.Directory = t.TempDir()
			retention.MaxAge = time.Hour
			retention.MaxWorkspaces = 4
			_, _ = (Dispatcher{Workspaces: manager, Backend: test.backend, Retention: retention}).Execute(test.context, validLease())
			if manager.cleaned == test.retained {
				t.Fatalf("cleaned = %v, retained = %v", manager.cleaned, test.retained)
			}
			records, err := filepath.Glob(filepath.Join(retention.Directory, "retained", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if (len(records) == 1) != test.retained {
				t.Fatalf("retention records = %#v, retained = %v", records, test.retained)
			}
		})
	}
}

// TestDispatcherCleansUpWorkspacesItCannotRegisterForRetention proves retention
// stays bounded: a workspace the sweep could never find again is not kept,
// because nothing would ever release it.
func TestDispatcherCleansUpWorkspacesItCannotRegisterForRetention(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{err: errors.New("agent exited")}, Retention: RetentionPolicy{KeepFailed: true}}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err == nil {
		t.Fatal("Execute() succeeded despite backend failure")
	}
	if !manager.cleaned {
		t.Fatal("an unregisterable workspace was retained without a bound")
	}
}

// TestDispatcherKeepsForensicsWhenTheBranchCannotBeReleased pins the trade-off:
// detaching is a guard against a collision that preparation already avoids, so
// failing to detach must not cost the run its forensics.
func TestDispatcherKeepsForensicsWhenTheBranchCannotBeReleased(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t), releaseErr: errors.New("worktree is locked")}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    &backend{err: errors.New("agent exited")},
		Retention:  RetentionPolicy{KeepFailed: true, Directory: t.TempDir(), MaxAge: time.Hour, MaxWorkspaces: 4},
	}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err == nil {
		t.Fatal("Execute() succeeded despite backend failure")
	}
	if manager.cleaned || manager.released {
		t.Fatalf("cleaned = %v, released = %v", manager.cleaned, manager.released)
	}
}

// TestDispatcherReleasesTheExecutionBranchOfRetainedWorkspaces guards the
// interaction between retention and workspace preparation: git refuses to
// re-create a branch that another worktree holds, and the orchestrator reuses
// one branch name for every execution of a workflow.
func TestDispatcherReleasesTheExecutionBranchOfRetainedWorkspaces(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    &backend{err: errors.New("agent exited")},
		Retention:  RetentionPolicy{KeepFailed: true, Directory: t.TempDir(), MaxAge: time.Hour, MaxWorkspaces: 4},
	}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err == nil {
		t.Fatal("Execute() succeeded despite backend failure")
	}
	if manager.cleaned || !manager.released {
		t.Fatalf("cleaned = %v, released = %v", manager.cleaned, manager.released)
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
	err    error
	// scopes records what each call was told about the job, so a test can
	// assert the lease actually reaches the resolver.
	scopes *[]SecretScope
}

func (resolver environmentResolver) Resolve(_ context.Context, scope SecretScope, _ []taskpacket.EnvironmentRef) (map[string]string, error) {
	if resolver.scopes != nil {
		*resolver.scopes = append(*resolver.scopes, scope)
	}
	return resolver.values, resolver.err
}

func TestDispatcherResolvesTaskEnvironmentBeforePreparingTheWorkspace(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{
		commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"},
		pushResult:   repository.PushResult{Branch: "agent/issue-7/run-1", Pushed: true},
	}
	lease := deliverableLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Delivery:           delivery,
		Environment:        environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}},
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if manager.prepared.Environment["GITHUB_TOKEN"] != "token-value" {
		t.Fatalf("prepare request environment = %#v", manager.prepared.Environment)
	}
	if delivery.pushEnv["GITHUB_TOKEN"] != "token-value" {
		t.Fatalf("push environment = %#v", delivery.pushEnv)
	}
}

func TestDispatcherFailsBeforePreparingWhenDeclaredEnvironmentIsUnresolvable(t *testing.T) {
	tests := []struct {
		name       string
		allowed    []string
		resolver   EnvironmentResolver
		wantErrors []string
	}{
		{
			name:       "not allowed on this runner",
			resolver:   environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}},
			wantErrors: []string{"GITHUB_TOKEN", "not allowed"},
		},
		{
			name:       "not configured on this runner",
			allowed:    []string{"GITHUB_TOKEN"},
			resolver:   environmentResolver{err: errors.New(`environment reference "GITHUB_TOKEN" is not configured on this runner`)},
			wantErrors: []string{"GITHUB_TOKEN", "not configured"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &workspaceManager{workspace: testWorkspace(t)}
			delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}}
			agent := &backend{result: agents.Result{Status: "completed"}}
			lease := deliverableLease()
			lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}

			_, err := (Dispatcher{Workspaces: manager, Backend: agent, Delivery: delivery, Environment: test.resolver, AllowedEnvironment: test.allowed}).Execute(context.Background(), lease)
			if err == nil {
				t.Fatal("Execute() accepted an unresolvable environment reference")
			}
			for _, want := range test.wantErrors {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Execute() error = %v, want it to mention %q", err, want)
				}
			}
			if manager.prepared.ProjectID != "" || manager.cleaned {
				t.Fatalf("workspace was prepared despite an unresolvable reference: %#v", manager.prepared)
			}
			if agent.request.ExecutionID != "" || len(delivery.commits) != 0 || len(delivery.pushes) != 0 {
				t.Fatalf("execution continued unauthenticated: agent = %#v, delivery = %#v", agent.request, delivery)
			}
		})
	}
}

func TestDispatcherPassesResolvedEnvironmentToEveryPipelinePath(t *testing.T) {
	for _, role := range []taskpacket.Role{taskpacket.RolePipeline, taskpacket.RoleDeveloper} {
		t.Run(string(role), func(t *testing.T) {
			manager := &workspaceManager{workspace: testWorkspace(t)}
			lease := validLease()
			lease.Packet.Role = role
			lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "TOKEN", SecretRef: "secret/token"}}
			lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "true", TimeoutSeconds: 1}}
			runner := &pipelineRunner{}
			dispatcher := Dispatcher{
				Workspaces:         manager,
				Backend:            &backend{result: agents.Result{Status: "completed"}},
				Pipeline:           runner,
				Environment:        environmentResolver{values: map[string]string{"TOKEN": "resolved"}},
				AllowedEnvironment: []string{"TOKEN"},
			}
			if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
				t.Fatal(err)
			}
			if runner.environment["TOKEN"] != "resolved" {
				t.Fatalf("pipeline environment = %#v", runner.environment)
			}
		})
	}
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

	_, err := (Dispatcher{Workspaces: manager, Backend: agent, AllowedEnvironment: []string{"TOKEN"}}).Execute(context.Background(), lease)
	if err == nil || manager.cleaned || manager.prepared.ProjectID != "" || agent.request.ExecutionID != "" {
		t.Fatalf("Execute() = %v, cleaned = %v, prepared = %#v, request = %#v", err, manager.cleaned, manager.prepared, agent.request)
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

type deliveryManager struct {
	mu                    sync.Mutex
	commitResult          repository.CommitResult
	commitErr             error
	pushResult            repository.PushResult
	pushErr               error
	workInProgressPushErr error
	recordErr             error
	commits               []string
	pushes                []string
	workInProgressPushes  []string
	recordedReferences    []string
	pushEnv               map[string]string
	workInProgressEnv     map[string]string
}

func (manager *deliveryManager) Commit(_ context.Context, _ repository.Workspace, message string) (repository.CommitResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.commits = append(manager.commits, message)
	return manager.commitResult, manager.commitErr
}

func (manager *deliveryManager) Push(_ context.Context, _ repository.Workspace, branch string, environment map[string]string) (repository.PushResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pushes = append(manager.pushes, branch)
	manager.pushEnv = environment
	return manager.pushResult, manager.pushErr
}

func (manager *deliveryManager) RecordWorkInProgress(_ context.Context, _ repository.Workspace, reference string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.recordedReferences = append(manager.recordedReferences, reference)
	return manager.recordErr
}

func (manager *deliveryManager) PushWorkInProgress(_ context.Context, _ repository.Workspace, branch string, environment map[string]string) (repository.PushResult, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.workInProgressPushes = append(manager.workInProgressPushes, branch)
	manager.workInProgressEnv = environment
	if manager.workInProgressPushErr != nil {
		return repository.PushResult{}, manager.workInProgressPushErr
	}
	return repository.PushResult{Branch: branch, Pushed: true}, nil
}

func deliverableLease() control.Lease {
	lease := validLease()
	lease.Packet.Constraints = taskpacket.Constraints{MayModifyFiles: true, MayPush: true}
	return lease
}

func TestDispatcherCommitsAndPushesWhenPacketPermitsModification(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{
		commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"},
		pushResult:   repository.PushResult{Branch: "agent/issue-7/run-1", Pushed: true},
	}
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}, Delivery: delivery}
	result, err := dispatcher.Execute(context.Background(), deliverableLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.Committed || result.FinalRevision != "deadbeef" || !result.Pushed || result.Branch != "agent/issue-7/run-1" {
		t.Fatalf("result = %#v", result)
	}
	if len(delivery.commits) != 1 || len(delivery.pushes) != 1 || delivery.pushes[0] != "agent/issue-7/run-1" {
		t.Fatalf("delivery calls = %#v", delivery)
	}
}

func TestDispatcherSkipsPushWhenCommitProducesNoNewRevision(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: false, Revision: "unchanged"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}, Delivery: delivery}
	result, err := dispatcher.Execute(context.Background(), deliverableLease())
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Committed || result.Pushed || len(delivery.pushes) != 0 {
		t.Fatalf("result = %#v, delivery = %#v", result, delivery)
	}
}

func TestDispatcherFailsClosedWithoutDeliveryManagerWhenModifyingFiles(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}}
	result, err := dispatcher.Execute(context.Background(), deliverableLease())
	if err == nil || result.Status != "failed" {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
}

// TestDispatcherRetainsWorkInProgressWithoutDeliveringWhenExecutionFails covers
// the delivery-safety contract for a non-delivering run: the work survives on a
// per-execution work-in-progress branch, and the deliverable agent branch is
// never written to.
func TestDispatcherRetainsWorkInProgressWithoutDeliveringWhenExecutionFails(t *testing.T) {
	tests := []struct {
		name    string
		backend *backend
	}{
		{name: "agent failure", backend: &backend{err: errors.New("agent exited")}},
		{name: "blocked agent", backend: &backend{result: agents.Result{Status: "blocked", Summary: "needs a decision"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &workspaceManager{workspace: testWorkspace(t)}
			delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}}
			dispatcher := Dispatcher{Workspaces: manager, Backend: test.backend, Delivery: delivery, PushWorkInProgress: true}

			result, _ := dispatcher.Execute(context.Background(), deliverableLease())
			if len(delivery.pushes) != 0 || result.Pushed || result.Branch != "" {
				t.Fatalf("failed execution delivered to the agent branch: result = %#v, pushes = %#v", result, delivery.pushes)
			}
			if test.backend.result.Status == "blocked" && (result.Status != "blocked" || result.Summary != "needs a decision") {
				t.Fatalf("a blocked agent result was flattened by the dispatcher: %#v", result)
			}
			if len(delivery.commits) != 1 || !strings.HasPrefix(delivery.commits[0], "wip(") {
				t.Fatalf("work in progress commit messages = %#v", delivery.commits)
			}
			if result.WorkInProgressCommit != "deadbeef" || result.WorkInProgressBranch != "wip/execution-1" || !result.WorkInProgressPushed {
				t.Fatalf("result = %#v", result)
			}
			if len(delivery.workInProgressPushes) != 1 || delivery.workInProgressPushes[0] != "wip/execution-1" {
				t.Fatalf("work in progress pushes = %#v", delivery.workInProgressPushes)
			}
		})
	}
}

// TestDispatcherSkipsThePipelineWhenTheAgentReportedABlock: the pipeline
// verdict would overwrite the agent's status with a generic `failed`, erasing
// the reason and the remaining work the block exists to carry. An agent that
// stopped without delivering has nothing worth validating anyway.
func TestDispatcherSkipsThePipelineWhenTheAgentReportedABlock(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}}
	lease := deliverableLease()
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "go test ./...", TimeoutSeconds: 5}}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend: &backend{result: agents.Result{
			Status:        "blocked",
			Summary:       "needs a decision on the schema",
			RemainingWork: []string{"decide the schema"},
		}},
		Delivery: delivery,
		Pipeline: &pipelineRunner{
			results: []pipeline.Result{{Command: "go test ./...", ExitCode: 1}},
			err:     errors.New("pipeline command failed with exit code 1: go test ./..."),
		},
	}

	result, err := dispatcher.Execute(context.Background(), lease)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a blocked result rather than a failure", err)
	}
	if result.Status != "blocked" || result.Summary != "needs a decision on the schema" {
		t.Fatalf("the pipeline overwrote the agent's block: %#v", result)
	}
	if len(result.RemainingWork) != 1 || result.RemainingWork[0] != "decide the schema" {
		t.Fatalf("remaining work = %#v", result.RemainingWork)
	}
	if len(result.PipelineResults) != 0 {
		t.Fatalf("the pipeline ran for a blocked agent result: %#v", result.PipelineResults)
	}
}

func TestDispatcherRecordsPipelineFailureBranchCommitAndLogTail(t *testing.T) {
	workspace := testWorkspace(t)
	manager := &workspaceManager{workspace: workspace}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "cafebabe"}}
	lease := deliverableLease()
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "go test ./...", TimeoutSeconds: 5}}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    &backend{result: agents.Result{Status: "completed"}},
		Delivery:   delivery,
		Pipeline: &pipelineRunner{
			results: []pipeline.Result{{Command: "go test ./...", ExitCode: 1, Output: "\x1b[31mpipeline output: FAIL\x1b[0m\n"}},
			err:     errors.New("pipeline command failed with exit code 1: go test ./..."),
		},
		PushWorkInProgress: true,
		Retention:          RetentionPolicy{KeepFailed: true, Directory: t.TempDir(), MaxAge: time.Hour, MaxWorkspaces: 4},
	}

	result, err := dispatcher.Execute(context.Background(), lease)
	if err == nil || result.Status != "failed" {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
	if result.WorkInProgressCommit != "cafebabe" || result.WorkInProgressBranch != "wip/execution-1" || !result.WorkInProgressPushed {
		t.Fatalf("pipeline failure did not retain its work: %#v", result)
	}
	if len(delivery.pushes) != 0 {
		t.Fatalf("pipeline failure pushed the agent branch: %#v", delivery.pushes)
	}
	if result.LogTail != "pipeline output: FAIL" {
		t.Fatalf("log tail = %q, want the sanitised pipeline output", result.LogTail)
	}
	if manager.cleaned {
		t.Fatal("failed workspace was cleaned up despite the retention policy")
	}
	contents, err := os.ReadFile(filepath.Join(workspace.Loop, "terminal-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Result
	if err := json.Unmarshal(contents, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.WorkInProgressBranch != "wip/execution-1" || persisted.WorkInProgressCommit != "cafebabe" || persisted.LogTail == "" {
		t.Fatalf("terminal result = %#v", persisted)
	}
}

func TestDispatcherKeepsWorkInProgressLocalWhenPushingIsNotPermitted(t *testing.T) {
	tests := []struct {
		name    string
		mayPush bool
		enabled bool
	}{
		{name: "packet forbids pushing", enabled: true},
		{name: "runner disables work in progress pushes", mayPush: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &workspaceManager{workspace: testWorkspace(t)}
			delivery := &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}}
			lease := deliverableLease()
			lease.Packet.Constraints.MayPush = test.mayPush
			dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{err: errors.New("agent exited")}, Delivery: delivery, PushWorkInProgress: test.enabled}

			result, _ := dispatcher.Execute(context.Background(), lease)
			if result.WorkInProgressCommit != "deadbeef" || result.WorkInProgressBranch != "" || result.WorkInProgressPushed {
				t.Fatalf("result = %#v", result)
			}
			if len(delivery.workInProgressPushes) != 0 {
				t.Fatalf("work in progress pushes = %#v", delivery.workInProgressPushes)
			}
			// Unpublished work still has to survive the branch reset the next
			// preparation of this job performs, or committing it achieved nothing.
			if len(delivery.recordedReferences) != 1 || delivery.recordedReferences[0] != "refs/moirai-wip/execution-1" {
				t.Fatalf("recorded references = %#v", delivery.recordedReferences)
			}
		})
	}
}

// TestDispatcherAnchorsCompletedWorkThatMayNotPush verifies issue #167: a
// completed file-modifying execution whose role lacks `mayPush` (e.g. repairer)
// commits on the execution branch but never publishes. The commit must be
// anchored outside refs/heads so the next preparation cannot destroy it.
func TestDispatcherAnchorsCompletedWorkThatMayNotPush(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{
		commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"},
	}
	lease := deliverableLease()
	lease.Packet.Constraints.MayPush = false
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{result: agents.Result{Status: "completed"}}, Delivery: delivery}

	result, err := dispatcher.Execute(context.Background(), lease)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || !result.Committed || result.Pushed || result.Branch != "" {
		t.Fatalf("completed run reported incorrect delivery state: %#v", result)
	}
	if len(delivery.recordedReferences) != 1 || delivery.recordedReferences[0] != "refs/moirai-wip/execution-1" {
		t.Fatalf("completed work was not anchored: recorded references = %#v", delivery.recordedReferences)
	}
	if len(delivery.commits) != 1 || len(delivery.pushes) != 0 || len(delivery.workInProgressPushes) != 0 {
		t.Fatalf("delivery calls = commits=%#v, pushes=%#v, wipPushes=%#v", delivery.commits, delivery.pushes, delivery.workInProgressPushes)
	}
}

// TestDispatcherReportsTheOriginalFailureWhenRetainingWorkFails proves that
// preserving a failed run's remains never replaces the failure the orchestrator
// has to see.
func TestDispatcherReportsTheOriginalFailureWhenRetainingWorkFails(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitErr: errors.New("worktree is locked")}
	dispatcher := Dispatcher{Workspaces: manager, Backend: &backend{err: errors.New("agent exited")}, Delivery: delivery, PushWorkInProgress: true}

	result, err := dispatcher.Execute(context.Background(), deliverableLease())
	if err == nil || !strings.Contains(err.Error(), "agent exited") {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.WorkInProgressCommit != "" || result.Status != "failed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestDispatcherSkipsWorkInProgressPushWhenNothingWasProduced(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	delivery := &deliveryManager{commitResult: repository.CommitResult{Revision: "initial"}}
	inspector := &revisionInspector{summaries: []repository.RevisionSummary{{Revision: "initial"}, {Revision: "initial"}}}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            &backend{err: errors.New("agent exited")},
		Delivery:           delivery,
		RevisionInspector:  inspector,
		PushWorkInProgress: true,
	}

	result, _ := dispatcher.Execute(context.Background(), deliverableLease())
	if result.WorkInProgressCommit != "" || len(delivery.workInProgressPushes) != 0 {
		t.Fatalf("result = %#v, pushes = %#v", result, delivery.workInProgressPushes)
	}
}

type streamingBackend struct {
	chunks []string
}

func (streamingBackend) Name() string                      { return "streaming" }
func (streamingBackend) HealthCheck(context.Context) error { return nil }
func (streamingBackend) Cancel(string) error               { return nil }
func (backend *streamingBackend) Execute(_ context.Context, request agents.Request) (agents.Result, error) {
	for _, chunk := range backend.chunks {
		if request.Output != nil {
			_, _ = request.Output.Write([]byte(chunk))
		}
	}
	return agents.Result{Status: "completed"}, nil
}
func (backend *streamingBackend) Continue(ctx context.Context, request agents.Request) (agents.Result, error) {
	return backend.Execute(ctx, request)
}

func TestDispatcherStreamsAgentOutputAsLogEvents(t *testing.T) {
	manager := &workspaceManager{workspace: testWorkspace(t)}
	var mu sync.Mutex
	var emitted []string
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    &streamingBackend{chunks: []string{"building...\n", "done\n"}},
		EmitLog: func(jobID string, generation int64, message string) ([]int64, error) {
			mu.Lock()
			defer mu.Unlock()
			emitted = append(emitted, message)
			return []int64{1}, nil
		},
	}
	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(emitted, "") != "building...\ndone\n" {
		t.Fatalf("emitted log chunks = %#v", emitted)
	}
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

type callbackBackend struct {
	onExecute func()
}

func (callbackBackend) Name() string                      { return "callback" }
func (callbackBackend) HealthCheck(context.Context) error { return nil }
func (callbackBackend) Cancel(string) error               { return nil }
func (agent *callbackBackend) Execute(context.Context, agents.Request) (agents.Result, error) {
	if agent.onExecute != nil {
		agent.onExecute()
	}
	return agents.Result{Status: "completed"}, nil
}
func (agent *callbackBackend) Continue(ctx context.Context, request agents.Request) (agents.Result, error) {
	return agent.Execute(ctx, request)
}

// discardingResolver records the jobs whose key material it was asked to
// remove, so a test can assert the dispatcher actually asks.
type discardingResolver struct {
	environmentResolver
	discarded []string
}

func (r *discardingResolver) Resolve(ctx context.Context, scope SecretScope, refs []taskpacket.EnvironmentRef) (map[string]string, error) {
	return r.environmentResolver.Resolve(ctx, scope, refs)
}

func (r *discardingResolver) DiscardJobKeys(jobID string) error {
	r.discarded = append(r.discarded, jobID)
	return nil
}

func TestDispatcherTellsTheResolverWhichJobAndLeaseTheSecretsAreFor(t *testing.T) {
	// Without this the control-plane resolver cannot prove the runner still
	// holds the job, and the lease fence on the orchestrator is unusable.
	var scopes []SecretScope
	lease := deliverableLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}
	dispatcher := Dispatcher{
		Workspaces:         &workspaceManager{workspace: testWorkspace(t)},
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Delivery:           &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		Environment:        environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}, scopes: &scopes},
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(scopes) != 1 || scopes[0].JobID != lease.JobID || scopes[0].LeaseGeneration != lease.Generation {
		t.Fatalf("resolver was told %#v, want the lease's job and generation", scopes)
	}
}

func TestDispatcherDiscardsKeyMaterialWhenTheJobEnds(t *testing.T) {
	lease := deliverableLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}
	resolver := &discardingResolver{
		environmentResolver: environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}},
	}
	dispatcher := Dispatcher{
		Workspaces:         &workspaceManager{workspace: testWorkspace(t)},
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Delivery:           &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		Environment:        resolver,
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(resolver.discarded) != 1 || resolver.discarded[0] != lease.JobID {
		t.Fatalf("discarded = %#v, want the job once", resolver.discarded)
	}
}

func TestDispatcherDiscardsKeyMaterialEvenWhenTheExecutionFails(t *testing.T) {
	// A key left on the tmpfs after a failure would be readable by the next
	// execution, which was never granted it.
	lease := deliverableLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}
	resolver := &discardingResolver{
		environmentResolver: environmentResolver{values: map[string]string{"GITHUB_TOKEN": "token-value"}},
	}
	dispatcher := Dispatcher{
		Workspaces:         &workspaceManager{prepareErr: errors.New("prepare exploded")},
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Environment:        resolver,
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err == nil {
		t.Fatal("Execute() succeeded, want the prepare failure")
	}
	if len(resolver.discarded) != 1 || resolver.discarded[0] != lease.JobID {
		t.Fatalf("discarded = %#v, want the job once", resolver.discarded)
	}
}

func TestDispatcherDiscardsKeyMaterialWhenResolutionItselfFails(t *testing.T) {
	// Resolution can write one key and then fail on the next reference.
	lease := deliverableLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}
	resolver := &discardingResolver{
		environmentResolver: environmentResolver{err: errors.New("control plane unreachable")},
	}
	dispatcher := Dispatcher{
		Workspaces:         &workspaceManager{workspace: testWorkspace(t)},
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Environment:        resolver,
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err == nil {
		t.Fatal("Execute() succeeded, want the resolution failure")
	}
	if len(resolver.discarded) != 1 {
		t.Fatalf("discarded = %#v, want the job once", resolver.discarded)
	}
}
