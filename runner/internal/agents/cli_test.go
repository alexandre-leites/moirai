package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/execution"
)

func TestCLIBackendExecutesConfiguredArgumentsAndReadsResult(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"implemented"}' > .loop/result.json
`)
	backend := CLIBackend{NameValue: "custom", Binary: binary, Arguments: []string{"run", "--safe"}, Supervisor: execution.NewSupervisor()}
	result, err := backend.Execute(context.Background(), Request{ExecutionID: "execution-1", Workspace: workspace, Prompt: "implement", Timeout: time.Second})
	if err != nil || result.Status != "completed" || result.Summary != "implemented" {
		t.Fatalf("Execute() = (%#v, %v)", result, err)
	}
	arguments, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil || strings.TrimSpace(string(arguments)) != "run\n--safe\nimplement" {
		t.Fatalf("arguments = %q, err = %v", arguments, err)
	}
}

func TestCLIBackendRejectsMissingIdentity(t *testing.T) {
	if err := (CLIBackend{}).HealthCheck(context.Background()); err == nil {
		t.Fatal("HealthCheck() accepted missing identity")
	}
}
