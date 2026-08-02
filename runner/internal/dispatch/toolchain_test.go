package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/execution"
	"github.com/loop-engineering/runner/internal/repository"
)

const testToolchainManifest = `{
  "schemaVersion": "1.0",
  "image": "moirai-runner",
  "summary": "The image under test.",
  "tools": [{"name": "git", "purpose": "Version control."}],
  "absent": [{"name": "python3", "note": "No Python runtime."}],
  "notes": ["Trust this list instead of probing."]
}`

// declaresToolchain points the dispatcher at a manifest written for the test,
// standing in for the one an execution image publishes at the conventional path.
func declaresToolchain(t *testing.T, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolchain.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	previous := toolchainManifestPath
	toolchainManifestPath = path
	t.Cleanup(func() { toolchainManifestPath = previous })
}

// declaresNoToolchain is a runner on a host, or an image built before the
// convention: there is nothing to declare and the agent must be told nothing
// rather than something invented.
func declaresNoToolchain(t *testing.T) {
	t.Helper()
	previous := toolchainManifestPath
	toolchainManifestPath = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { toolchainManifestPath = previous })
}

// The whole point of the issue: the agent is told what it has, in the prompt it
// reads, before it can spend an attempt finding out by running `python3`.
func TestAgentPromptDeclaresTheLocalToolchain(t *testing.T) {
	declaresToolchain(t, testToolchainManifest)
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{result: agents.Result{Status: "completed", Summary: "done"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, want := range []string{"# EXECUTION ENVIRONMENT", "moirai-runner", "`git`", "Not installed:", "`python3`", "Trust this list instead of probing."} {
		if !strings.Contains(agent.request.Prompt, want) {
			t.Fatalf("agent prompt is missing %q:\n%s", want, agent.request.Prompt)
		}
	}
	// The prompt the harness is handed and the prompt artifact on disk are two
	// different deliveries of the same text, and opencode only ever reads the
	// second one. A declaration that reached only the first would look correct
	// in this test's request and be invisible to the default backend.
	if artifact := manager.artifacts["prompt.md"]; !strings.Contains(artifact, "# EXECUTION ENVIRONMENT") || !strings.Contains(artifact, "`python3`") {
		t.Fatalf("prompt artifact does not declare the toolchain:\n%s", artifact)
	}
	// The control plane's half of the prompt still has to survive intact.
	if !strings.Contains(manager.artifacts["prompt.md"], "# IMMUTABLE OBJECTIVE") {
		t.Fatalf("appending the declaration lost the task prompt:\n%s", manager.artifacts["prompt.md"])
	}
}

func TestAgentPromptIsUnchangedWhenTheEnvironmentDeclaresNothing(t *testing.T) {
	declaresNoToolchain(t)
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{result: agents.Result{Status: "completed", Summary: "done"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if agent.request.Prompt != promptFor(validLease().Packet) {
		t.Fatalf("prompt changed with no manifest to declare:\n%s", agent.request.Prompt)
	}
	if strings.Contains(manager.artifacts["prompt.md"], "# EXECUTION ENVIRONMENT") {
		t.Fatalf("prompt artifact gained an empty declaration:\n%s", manager.artifacts["prompt.md"])
	}
}

// The failure that would be worse than saying nothing: an agent running inside
// a project's own execution image, told about the runner's filesystem. It is a
// different machine, so it gets the conventional path instead of contents.
func TestExecutionImageIsDeclaredWithoutTheRunnersOwnToolchain(t *testing.T) {
	declaresToolchain(t, testToolchainManifest)
	dispatcher := Dispatcher{Backend: &backend{}}
	packet := validLease().Packet
	packet.ExecutionImage = "ghcr.io/example/toolchain:1"

	declaration := dispatcher.environmentDeclaration(packet)
	if !strings.Contains(declaration, "ghcr.io/example/toolchain:1") || !strings.Contains(declaration, "/etc/moirai/toolchain.json") {
		t.Fatalf("execution image declaration = %q", declaration)
	}
	for _, unwanted := range []string{"moirai-runner", "`python3`"} {
		if strings.Contains(declaration, unwanted) {
			t.Fatalf("execution image declaration describes the runner instead: %q", declaration)
		}
	}
}

// Same reasoning for the operator-configured container backend, which no packet
// field describes: the agent is not in the runner's filesystem either.
func TestContainerBackendIsDeclaredWithoutTheRunnersOwnToolchain(t *testing.T) {
	declaresToolchain(t, testToolchainManifest)
	dispatcher := Dispatcher{Backend: agents.DockerCLIBackend{Image: "example/agent:2", Executor: execution.DockerExecutor{}}}

	declaration := dispatcher.environmentDeclaration(validLease().Packet)
	if !strings.Contains(declaration, "example/agent:2") || strings.Contains(declaration, "moirai-runner") {
		t.Fatalf("container backend declaration = %q", declaration)
	}
}

// A manifest that exists but is broken is a defect in the image. The agent is
// told nothing rather than something wrong, and the job still runs.
func TestUnusableManifestLeavesThePromptAloneWithoutFailingTheRun(t *testing.T) {
	declaresToolchain(t, `{"schemaVersion":"9.9"}`)
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &backend{result: agents.Result{Status: "completed", Summary: "done"}}
	dispatcher := Dispatcher{Workspaces: manager, Backend: agent}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(agent.request.Prompt, "# EXECUTION ENVIRONMENT") {
		t.Fatalf("a broken manifest reached the agent:\n%s", agent.request.Prompt)
	}
}

// A prompt artifact that cannot be extended degrades the agent; it must not
// fail the job, and it must not conjure the file it was meant to append to.
func TestAppendingToAnUnwritablePromptArtifactIsBestEffort(t *testing.T) {
	packet := validLease().Packet
	root := t.TempDir()

	appendPromptSection(repository.Workspace{}, packet, "section")
	appendPromptSection(repository.Workspace{Repository: root}, packet, "section")

	if _, err := os.Stat(filepath.Join(root, packet.PromptPath)); !os.IsNotExist(err) {
		t.Fatalf("appending created a prompt artifact that writeArtifacts never wrote: %v", err)
	}
}

// A continuation is a fresh prompt for every backend that takes one on its
// command line. Telling the agent what it has on the first attempt and not on
// the second would put it back to probing halfway through an execution.
func TestContinuationPromptKeepsTheEnvironmentDeclaration(t *testing.T) {
	declaresToolchain(t, testToolchainManifest)
	manager := &workspaceManager{workspace: testWorkspace(t)}
	agent := &scriptedBackend{turns: []scriptedTurn{
		{result: agents.Result{Status: "completed", Summary: "wrote the parser", RemainingWork: []string{"write the tests"}, SessionID: "session-1"}},
		{result: agents.Result{Status: "completed", Summary: "tests written", SessionID: "session-1"}},
	}}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          agent,
		Delivery:         &deliveryManager{commitResult: repository.CommitResult{Committed: true, Revision: "deadbeef"}},
		MaxContinuations: 3,
		RevisionInspector: &repeatingInspector{summaries: []repository.RevisionSummary{
			{Revision: "base"},
			{Revision: "base", ChangedFiles: []string{"parser.go"}},
		}},
	}

	if _, err := dispatcher.Execute(context.Background(), developerLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(agent.requests) != 2 {
		t.Fatalf("agent invocations = %d", len(agent.requests))
	}
	for index, request := range agent.requests {
		if !strings.Contains(request.Prompt, "# EXECUTION ENVIRONMENT") || !strings.Contains(request.Prompt, "`python3`") {
			t.Fatalf("invocation %d lost the environment declaration:\n%s", index, request.Prompt)
		}
	}
	// Resolved once for the execution, not re-read for every attempt: the
	// declaration reaching both prompts must not mean two reads of the manifest.
	if count := strings.Count(agent.requests[1].Prompt, "# EXECUTION ENVIRONMENT"); count != 1 {
		t.Fatalf("continuation prompt declares the environment %d times", count)
	}
}
