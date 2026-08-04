package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

// concurrentWorkspaceManager hands every execution its own directory tree, so
// several Execute calls can run against one shared dispatcher the way the
// control loop runs them (one goroutine per lease, same *Dispatcher).
type concurrentWorkspaceManager struct {
	root string

	mu         sync.Mutex
	workspaces map[string]repository.Workspace
}

func newConcurrentWorkspaceManager(root string) *concurrentWorkspaceManager {
	return &concurrentWorkspaceManager{root: root, workspaces: map[string]repository.Workspace{}}
}

func (manager *concurrentWorkspaceManager) Prepare(_ context.Context, request repository.PrepareRequest) (repository.Workspace, error) {
	root := filepath.Join(manager.root, request.JobID)
	workspace := repository.Workspace{
		Root:       root,
		Repository: filepath.Join(root, "repository"),
		Loop:       filepath.Join(root, "repository", ".loop"),
	}
	for _, path := range []string{workspace.Root, workspace.Repository, workspace.Loop} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return repository.Workspace{}, err
		}
	}
	manager.mu.Lock()
	manager.workspaces[request.JobID] = workspace
	manager.mu.Unlock()
	return workspace, nil
}

func (manager *concurrentWorkspaceManager) Cleanup(context.Context, string, string) error { return nil }

func (manager *concurrentWorkspaceManager) CleanupExisting(context.Context, string, string) error {
	return nil
}

func (manager *concurrentWorkspaceManager) ReleaseBranch(context.Context, repository.Workspace) error {
	return nil
}

// imageBackend reports which execution image it was built for, so a test can
// tell which backend an execution actually ran on.
type imageBackend struct {
	image string

	mu    sync.Mutex
	calls []string
}

func (agent *imageBackend) Name() string                      { return "image-" + agent.image }
func (agent *imageBackend) HealthCheck(context.Context) error { return nil }
func (agent *imageBackend) Cancel(string) error               { return nil }

func (agent *imageBackend) Execute(_ context.Context, request agents.Request) (agents.Result, error) {
	agent.mu.Lock()
	agent.calls = append(agent.calls, request.ExecutionID)
	agent.mu.Unlock()
	return agents.Result{Status: "completed", Summary: agent.image}, nil
}

func (agent *imageBackend) Continue(ctx context.Context, request agents.Request) (agents.Result, error) {
	return agent.Execute(ctx, request)
}

func (agent *imageBackend) executions() []string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]string(nil), agent.calls...)
}

// imagePipeline reports which execution image its runner was built for.
type imagePipeline struct {
	image string

	mu   sync.Mutex
	runs int
}

func (runner *imagePipeline) Run(context.Context, string, map[string]string, []pipeline.Command) ([]pipeline.Result, error) {
	runner.mu.Lock()
	runner.runs++
	runner.mu.Unlock()
	return []pipeline.Result{{Command: runner.image, ExitCode: 0}}, nil
}

func (runner *imagePipeline) count() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.runs
}

func imageLease(index int) control.Lease {
	lease := validLease()
	lease.JobID = fmt.Sprintf("job-%d", index)
	lease.Packet.JobID = lease.JobID
	lease.Packet.ExecutionID = fmt.Sprintf("execution-%d", index)
	lease.Packet.ExecutionImage = fmt.Sprintf("ghcr.io/example/toolchain:%d", index)
	// Distinct projects so the concurrency guard, which serialises one project's
	// executions on purpose, does not serialise the whole test.
	lease.Packet.Repository.ProjectID = fmt.Sprintf("project-%d", index)
	lease.Packet.Repository.Branch = fmt.Sprintf("agent/issue-7/run-%d", index)
	lease.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "go test ./...", TimeoutSeconds: 60, Required: true}}
	return lease
}

// TestExecutionImageOverrideDoesNotReachTheSharedDispatcher pins the invariant
// that a packet's execution image replaces the agent backend and pipeline
// runner for that one execution only. The dispatcher itself is long-lived and
// shared, so an override written into it would follow every later execution.
func TestExecutionImageOverrideDoesNotReachTheSharedDispatcher(t *testing.T) {
	defaultBackend := &imageBackend{image: "default"}
	defaultPipeline := &imagePipeline{image: "default"}
	imaged := &imageBackend{image: "imaged"}
	imagedPipeline := &imagePipeline{image: "imaged"}
	dispatcher := &Dispatcher{
		Workspaces: newConcurrentWorkspaceManager(t.TempDir()),
		Backend:    defaultBackend,
		Pipeline:   defaultPipeline,
		ExecutionEnvironment: func(string) (agents.Backend, pipeline.Runner, error) {
			return imaged, imagedPipeline, nil
		},
	}

	if _, err := dispatcher.Execute(context.Background(), imageLease(1)); err != nil {
		t.Fatalf("Execute() with an execution image error = %v", err)
	}
	if dispatcher.Backend != agents.Backend(defaultBackend) || dispatcher.Pipeline != pipeline.Runner(defaultPipeline) {
		t.Fatalf("execution image override was written back into the shared dispatcher: backend = %v, pipeline = %v", dispatcher.Backend, dispatcher.Pipeline)
	}

	// The next execution carries no image, so it must land on the dispatcher's
	// own backend and pipeline rather than the previous run's overrides.
	plain := validLease()
	plain.JobID, plain.Packet.JobID = "job-plain", "job-plain"
	plain.Packet.ExecutionID = "execution-plain"
	plain.Packet.Pipeline = []taskpacket.PipelineCommand{{Command: "go test ./...", TimeoutSeconds: 60, Required: true}}
	if _, err := dispatcher.Execute(context.Background(), plain); err != nil {
		t.Fatalf("Execute() without an execution image error = %v", err)
	}
	if got := defaultBackend.executions(); len(got) != 1 || got[0] != "execution-plain" {
		t.Fatalf("dispatcher backend ran %v, want only the un-imaged execution", got)
	}
	if got := imaged.executions(); len(got) != 1 || got[0] != "execution-1" {
		t.Fatalf("image backend ran %v, want only the imaged execution", got)
	}
	if defaultPipeline.count() != 1 || imagedPipeline.count() != 1 {
		t.Fatalf("pipeline runs: dispatcher = %d, image = %d, want 1 each", defaultPipeline.count(), imagedPipeline.count())
	}
}

// TestConcurrentExecutionsKeepTheirOwnExecutionEnvironment is the regression
// guard the value receiver used to provide by accident. The control loop starts
// each lease in its own goroutine against a single shared *Dispatcher, so if
// Execute ever applied a packet's execution image to that shared dispatcher —
// which is what a pointer receiver would make it do — concurrent executions
// would run on each other's toolchain images, and the race detector would see
// the write. Both are asserted here.
func TestConcurrentExecutionsKeepTheirOwnExecutionEnvironment(t *testing.T) {
	const executions = 8
	backends := make([]*imageBackend, executions)
	pipelines := make([]*imagePipeline, executions)
	byImage := map[string]int{}
	for index := range backends {
		lease := imageLease(index)
		backends[index] = &imageBackend{image: lease.Packet.ExecutionImage}
		pipelines[index] = &imagePipeline{image: lease.Packet.ExecutionImage}
		byImage[lease.Packet.ExecutionImage] = index
	}

	shared := &imageBackend{image: "shared"}
	sharedPipeline := &imagePipeline{image: "shared"}
	dispatcher := &Dispatcher{
		Workspaces: newConcurrentWorkspaceManager(t.TempDir()),
		Backend:    shared,
		Pipeline:   sharedPipeline,
		Projects:   NewProjectConcurrencyGuard(),
		Active:     NewActiveWorkspaces(),
		ExecutionEnvironment: func(image string) (agents.Backend, pipeline.Runner, error) {
			index, known := byImage[image]
			if !known {
				return nil, nil, fmt.Errorf("unknown execution image %q", image)
			}
			return backends[index], pipelines[index], nil
		},
	}

	start := make(chan struct{})
	errs := make([]error, executions)
	var group sync.WaitGroup
	for index := range executions {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			_, err := dispatcher.Execute(context.Background(), imageLease(index))
			errs[index] = err
		}(index)
	}
	close(start)
	group.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("Execute() %d error = %v", index, err)
		}
	}
	for index, agent := range backends {
		want := fmt.Sprintf("execution-%d", index)
		if got := agent.executions(); len(got) != 1 || got[0] != want {
			t.Fatalf("backend for image %d ran %v, want exactly [%s]", index, got, want)
		}
		if runs := pipelines[index].count(); runs != 1 {
			t.Fatalf("pipeline for image %d ran %d times, want 1", index, runs)
		}
	}
	if got := shared.executions(); len(got) != 0 {
		t.Fatalf("the shared backend ran %v, want nothing: every packet named an execution image", got)
	}
	if runs := sharedPipeline.count(); runs != 0 {
		t.Fatalf("the shared pipeline ran %d times, want 0", runs)
	}
	if dispatcher.Backend != agents.Backend(shared) || dispatcher.Pipeline != pipeline.Runner(sharedPipeline) {
		t.Fatalf("concurrent executions mutated the shared dispatcher: backend = %v, pipeline = %v", dispatcher.Backend, dispatcher.Pipeline)
	}
}
