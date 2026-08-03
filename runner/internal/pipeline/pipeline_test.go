package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/execution"
)

func TestLocalRunnerRecordsSequentialResults(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{
		{Command: "printf first", Timeout: time.Second},
		{Command: "printf second", Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(results) != 2 || results[0].Output != "first" || results[1].Output != "second" || results[0].ExitCode != 0 {
		t.Fatalf("results = %#v", results)
	}
}

func TestLocalRunnerDoesNotInheritRunnerEnvironment(t *testing.T) {
	t.Setenv("RUNNER_SECRET", "must-not-reach-pipeline")
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), map[string]string{"DECLARED_VALUE": "allowed"}, []Command{{Command: "env", Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	environment := parseEnvironment(t, results[0].Output)
	if environment["RUNNER_SECRET"] != "" || environment["DECLARED_VALUE"] != "allowed" {
		t.Fatalf("pipeline environment = %#v", environment)
	}
	for _, name := range []string{"PATH", "HOME", "TMPDIR"} {
		if environment[name] == "" {
			t.Fatalf("pipeline environment missing %s: %#v", name, environment)
		}
	}
}

func TestRunnersUseSamePipelineEnvironment(t *testing.T) {
	workspace := t.TempDir()
	localResults, err := (LocalRunner{}).Run(context.Background(), workspace, map[string]string{"DECLARED_VALUE": "allowed"}, []Command{{Command: "env", Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	recorded := filepath.Join(directory, "environment")
	binary := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nprevious=\nfor argument in \"$@\"; do if [ \"$previous\" = --env-file ]; then cat \"$argument\" > \"" + recorded + "\"; fi; previous=\"$argument\"; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = (DockerRunner{Executor: execution.DockerExecutor{Binary: binary, Image: "example/pipeline:1"}}).Run(context.Background(), workspace, map[string]string{"DECLARED_VALUE": "allowed"}, []Command{{Command: "true", Timeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parseEnvironment(t, string(contents)), parseEnvironment(t, localResults[0].Output); !sameEnvironment(got, want) {
		t.Fatalf("Docker environment = %#v, local environment = %#v", got, want)
	}
}

func parseEnvironment(t *testing.T, output string) map[string]string {
	t.Helper()
	values := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		name, value, found := strings.Cut(line, "=")
		if !found {
			t.Fatalf("environment line = %q", line)
		}
		values[name] = value
	}
	return values
}

func sameEnvironment(first, second map[string]string) bool {
	if len(first) != len(second) {
		return false
	}
	for name, value := range first {
		if second[name] != value {
			return false
		}
	}
	return true
}

func TestLocalRunnerStopsAtFailureAndRecordsExitCode(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{
		{Command: "printf before", Timeout: time.Second, Required: true},
		{Command: "false", Timeout: time.Second, Required: true},
		{Command: "printf after", Timeout: time.Second, Required: true},
	})
	if err == nil || len(results) != 2 || results[1].ExitCode != 1 {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}

func TestLocalRunnerContinuesPastNonRequiredFailure(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{
		{Command: "printf before", Timeout: time.Second, Required: true},
		{Command: "false", Timeout: time.Second, Required: false},
		{Command: "printf after", Timeout: time.Second, Required: true},
	})
	if err != nil || len(results) != 3 || results[1].ExitCode != 1 || results[2].Output != "after" {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}

func TestLocalRunnerStopsAtRequiredFailureAfterNonRequiredOne(t *testing.T) {
	// The first command is non-required and fails; the pipeline must still run
	// the second, required one, and fail overall on *that* one specifically.
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{
		{Command: "false", Timeout: time.Second, Required: false},
		{Command: "false", Timeout: time.Second, Required: true},
	})
	if err == nil || len(results) != 2 || results[0].ExitCode != 1 || results[1].ExitCode != 1 {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}

func TestParseCommandTemplateRejectsShellSyntax(t *testing.T) {
	for _, command := range []string{"printf safe; rm -rf /", "echo $TOKEN", "printf one | cat", "echo 'quoted'"} {
		if _, err := ParseCommandTemplate(command); err == nil {
			t.Fatalf("ParseCommandTemplate(%q) accepted shell syntax", command)
		}
	}
	arguments, err := ParseCommandTemplate("printf safe")
	if err != nil || len(arguments) != 2 || arguments[1] != "safe" {
		t.Fatalf("ParseCommandTemplate() = %#v, %v", arguments, err)
	}
}

func TestLocalRunnerMarksTimeout(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{{Command: "sleep 1", Timeout: 10 * time.Millisecond, Required: true}})
	if err == nil || len(results) != 1 || !results[0].TimedOut || results[0].ExitCode != -1 {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}

func TestLocalRunnerContinuesPastNonRequiredTimeout(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), nil, []Command{
		{Command: "sleep 1", Timeout: 10 * time.Millisecond, Required: false},
		{Command: "printf after", Timeout: time.Second, Required: true},
	})
	if err != nil || len(results) != 2 || !results[0].TimedOut || results[1].Output != "after" {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}
