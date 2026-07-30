package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestOpenCodeBackendCarriesRawResultDocumentForRoleSpecificFields(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
cat > .loop/result.json <<'JSON'
{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"reviewed","verdict":"approved","findings":[],"acceptanceCriteria":[]}
JSON
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Role:        RoleReviewer,
		Workspace:   workspace,
		Prompt:      "review the task",
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Raw == nil || result.Raw["verdict"] != "approved" {
		t.Fatalf("Raw result = %#v", result.Raw)
	}
}

// TestOpenCodeBackendReportsACleanBlockWithItsRemainingWork is the source of
// the chain issue #97 restores: an agent that finished cleanly and declared
// itself blocked reports that status, its reason, and what it says still has to
// be done — none of it collapsed into a bare failure.
func TestOpenCodeBackendReportsACleanBlockWithItsRemainingWork(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
cat > .loop/result.json <<'JSON'
{"protocolVersion":"1.0","executionId":"execution-1","status":"blocked","summary":"the deployment credential is missing","changedFiles":["a.go"],"commandsRun":[],"remainingWork":["obtain DEPLOY_KEY","re-run the migration"],"sessionId":"session-1"}
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
		t.Fatalf("Execute() error = %v, want a blocked result rather than a failure", err)
	}
	if result.Status != "blocked" || result.Summary != "the deployment credential is missing" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(result.RemainingWork) != 2 || result.RemainingWork[0] != "obtain DEPLOY_KEY" {
		t.Fatalf("RemainingWork = %#v", result.RemainingWork)
	}
	if result.Raw == nil || result.Raw["status"] != "blocked" {
		t.Fatalf("Raw result = %#v", result.Raw)
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
	if err != nil || string(contents) != "run\n--auto\n--model\nprovider/model\n--dir\n"+workspace+"\nRead .loop/prompt.md and follow its instructions.\n" {
		t.Fatalf("arguments = %q, %v", contents, err)
	}
}

// A clean exit without usable evidence must never be reported as success: the
// agent may have ended its reasoning loop early, refused, or crashed its own
// logic while still exiting 0.
func TestOpenCodeBackendFailsWhenResultDocumentIsMissingOrInvalid(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		agent string
	}{
		{name: "missing document", agent: "mkdir -p .loop\n"},
		{name: "malformed json", agent: "mkdir -p .loop\nprintf '%s' '{\"protocolVersion\":' > .loop/result.json\n"},
		{name: "wrong execution id", agent: `mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-other","status":"completed","summary":"implemented"}' > .loop/result.json
`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			binary := writeFakeOpenCode(t, workspace, testCase.agent)
			backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
			result, err := backend.Execute(context.Background(), Request{
				ExecutionID: "execution-1",
				Workspace:   workspace,
				Prompt:      "implement the task",
				Timeout:     time.Second,
			})
			if !errors.Is(err, ErrNoResultEvidence) {
				t.Fatalf("Execute() error = %v, want ErrNoResultEvidence", err)
			}
			if result.Status != "failed" {
				t.Fatalf("Status = %q, want failed", result.Status)
			}
			if result.ExitCode != 0 {
				t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
			}
			if !strings.Contains(err.Error(), filepath.Join(".loop", "result.json")) {
				t.Fatalf("error %q does not name the missing evidence", err)
			}
			if result.Summary != err.Error() {
				t.Fatalf("Summary = %q, want %q", result.Summary, err)
			}
			if strings.Contains(err.Error(), workspace) {
				t.Fatalf("error %q leaks the absolute workspace path", err)
			}
		})
	}
}

// The failure fingerprint is derived from the error text, so two executions
// that fail the same way must produce identical text across workspaces.
func TestOpenCodeBackendMissingResultFailureTextIsStableAcrossWorkspaces(t *testing.T) {
	messages := make([]string, 0, 2)
	for range 2 {
		workspace := t.TempDir()
		binary := writeFakeOpenCode(t, workspace, "mkdir -p .loop\n")
		backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
		_, err := backend.Execute(context.Background(), Request{
			ExecutionID: "execution-1",
			Workspace:   workspace,
			Prompt:      "implement the task",
			Timeout:     time.Second,
		})
		if err == nil {
			t.Fatal("Execute() error = nil, want a failure")
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Fatalf("failure text differs across workspaces: %q vs %q", messages[0], messages[1])
	}
}

// A failing process keeps reporting the process failure so the orchestrator can
// tell it apart from an agent that exited cleanly with nothing to show.
func TestOpenCodeBackendReportsProcessFailureRatherThanMissingEvidence(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
exit 3
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "implement the task",
		Timeout:     time.Second,
	})
	if err == nil || errors.Is(err, ErrNoResultEvidence) {
		t.Fatalf("Execute() error = %v, want the process failure", err)
	}
	if result.Status != "failed" || result.ExitCode != 3 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestOpenCodeBackendKeepsDocumentStatusWhenProcessFails(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"blocked","summary":"needs credentials"}' > .loop/result.json
exit 3
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "implement the task",
		Timeout:     time.Second,
	})
	if err == nil || errors.Is(err, ErrNoResultEvidence) {
		t.Fatalf("Execute() error = %v, want the process failure", err)
	}
	if result.Status != "blocked" || result.Summary != "needs credentials" || result.ExitCode != 3 {
		t.Fatalf("unexpected result: %#v", result)
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
