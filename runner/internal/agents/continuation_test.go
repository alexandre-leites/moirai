package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/execution"
)

// A continuation resumes the session the previous run reported, using opencode's
// own session selector, and changes nothing else about the invocation.
func TestOpenCodeContinueResumesTheCapturedSession(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"finished"}' > .loop/result.json
`)
	backend := OpenCodeBackend{Binary: binary, Arguments: []string{"--model", "provider/model"}, Supervisor: execution.NewSupervisor()}
	if _, err := backend.Continue(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "continue the task",
		SessionID:   "session-1",
		Timeout:     time.Second,
	}); err != nil {
		t.Fatalf("Continue() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	want := "run\n--model\nprovider/model\n--dir\n" + workspace + "\n--session\nsession-1\ncontinue the task\n"
	if string(contents) != want {
		t.Fatalf("arguments = %q, want %q", contents, want)
	}
}

// An execution whose agent never reported a session — most often because it
// wrote no result document at all — is still continued, as a fresh run driven
// by the continuation prompt.
func TestOpenCodeContinueFallsBackToAFreshRunWithoutASession(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"finished"}' > .loop/result.json
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	if _, err := backend.Continue(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "continue the task",
		Timeout:     time.Second,
	}); err != nil {
		t.Fatalf("Continue() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	if strings.Contains(string(contents), "--session") {
		t.Fatalf("arguments = %q, want no session selector", contents)
	}
}

// Execute never resumes: a first run must start a fresh session even when the
// caller left a stale session ID on the request.
func TestOpenCodeExecuteNeverResumesASession(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"finished"}' > .loop/result.json
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	if _, err := backend.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "implement the task",
		SessionID:   "session-1",
		Timeout:     time.Second,
	}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	if strings.Contains(string(contents), "--session") {
		t.Fatalf("arguments = %q, want no session selector", contents)
	}
}

// A continuation must not erase the attempt that explains why it was issued.
// One execution keeps one log per stream, and its metadata describes all of it.
func TestAgentLogsAccumulateAcrossContinuations(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf 'attempt %s\n' "$#"
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"finished"}' > .loop/result.json
`)
	backend := OpenCodeBackend{Binary: binary, Supervisor: execution.NewSupervisor()}
	request := Request{ExecutionID: "execution-1", Workspace: workspace, Prompt: "implement the task", Timeout: time.Second}
	if _, err := backend.Execute(context.Background(), request); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	request.SessionID = "session-1"
	request.Prompt = "continue the task"
	if _, err := backend.Continue(context.Background(), request); err != nil {
		t.Fatalf("Continue() error = %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(workspace, ".loop", "opencode.stdout.log"))
	if err != nil {
		t.Fatalf("read agent log: %v", err)
	}
	if strings.Count(string(contents), "attempt") != 2 {
		t.Fatalf("agent log lost an attempt: %q", contents)
	}
	metadata, err := os.ReadFile(filepath.Join(workspace, ".loop", "opencode.log-metadata.json"))
	if err != nil {
		t.Fatalf("read log metadata: %v", err)
	}
	var decoded struct {
		StdoutBytes int `json:"stdoutBytes"`
	}
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatalf("decode log metadata: %v", err)
	}
	if decoded.StdoutBytes != len(contents) {
		t.Fatalf("stdoutBytes = %d, want the whole execution's %d", decoded.StdoutBytes, len(contents))
	}
}

// The CLI and Docker backends have no session to resume, so their contractual
// fallback is a fresh run carrying the continuation prompt.
func TestSessionlessBackendsFallBackToAFreshRun(t *testing.T) {
	workspace := t.TempDir()
	binary := writeFakeOpenCode(t, workspace, `printf '%s\n' "$@" > arguments
mkdir -p .loop
printf '%s' '{"protocolVersion":"1.0","executionId":"execution-1","status":"completed","summary":"finished"}' > .loop/result.json
`)
	backend := CLIBackend{NameValue: "cli", Binary: binary, Supervisor: execution.NewSupervisor()}
	result, err := backend.Continue(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Prompt:      "continue the task",
		SessionID:   "session-1",
		Timeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("Continue() error = %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "arguments"))
	if err != nil {
		t.Fatalf("read arguments: %v", err)
	}
	if string(contents) != "continue the task\n" {
		t.Fatalf("arguments = %q", contents)
	}
}

// Every configured backend satisfies the whole Backend contract, Continue
// included; a backend that could not be continued would silently disable the
// goal loop for whoever configured it. The slice literal is the assertion —
// this fails to compile if a backend loses Continue — and the body checks that
// each entry is a usable backend rather than a zero value that happens to
// satisfy the method set.
func TestEveryConfigurableBackendImplementsTheContract(t *testing.T) {
	backends := []Backend{
		OpenCodeBackend{},
		CLIBackend{NameValue: "cli", Binary: "true"},
		DockerCLIBackend{Image: "example"},
	}
	if len(backends) != 3 {
		t.Fatalf("backends = %d, want every configurable backend", len(backends))
	}
	for _, backend := range backends {
		if backend.Name() == "" {
			t.Fatalf("%T reported no name", backend)
		}
		// Continue must reject the same invalid request Execute does, rather
		// than being a stub that silently succeeds.
		if _, err := backend.Continue(context.Background(), Request{}); err == nil {
			t.Fatalf("%T.Continue accepted an empty request", backend)
		}
	}
}
