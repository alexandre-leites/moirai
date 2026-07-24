package agents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/execution"
)

func TestOpenCodeBackendReadsValidatedResult(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
cat > .loop/result.json <<'JSON'
{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"implemented","changedFiles":["a.go"],"commandsRun":["go test ./..."],"remainingWork":[],"sessionId":"session-1"}
JSON
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Role:        RoleDeveloper,
		Workspace:   workspace,
		Prompt:      "implement the task",
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" || result.Summary != "implemented" || result.SessionID != "session-1" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.ChangedFiles) != 1 || result.ChangedFiles[0] != "a.go" {
		t.Fatalf("ChangedFiles = %#v", result.ChangedFiles)
	}
}

func TestOpenCodeBackendPassesConfiguredArgumentsBeforePrompt(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"implemented"}' > .loop/result.json
`)
	backend := OpenCodeBackend{Binary: binary, Arguments: []string{"--auto", "--model", "provider/model"}, Supervisor: execution.NewSupervisor()}
	if _, err := backend.Execute(context.Background(), Request{ExecutionID: "execution-1", Workspace: workspace, Prompt: "implement the task", Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil || string(contents) != "run\n--auto\n--model\nprovider/model\n--dir\n"+workspace+"\nimplement the task\n" {
		t.Fatalf("arguments = %q, %v", contents, err)
	}
}

func TestOpenCodeBackendFallsBackOnMissingResult(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "implement the task",
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("Status = %q, want completed", result.Status)
	}
}

func TestResultPathMustRemainInWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if _, err := resultPathWithinWorkspace(workspace, "../result.json"); err == nil {
		t.Fatal("resultPathWithinWorkspace() accepted escaped path")
	}
}

func writeFakeOpenCode(t *testing.T, directory, body string) string {
	t.Helper()
	path := filepath.Join(directory, "fake-opencode")
	contents := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(contents), 0o750); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}
