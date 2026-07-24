package pipeline

import (
	"context"
	"testing"
	"time"
)

func TestLocalRunnerRecordsSequentialResults(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), []Command{
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

func TestLocalRunnerStopsAtFailureAndRecordsExitCode(t *testing.T) {
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), []Command{
		{Command: "printf before", Timeout: time.Second},
		{Command: "false", Timeout: time.Second},
		{Command: "printf after", Timeout: time.Second},
	})
	if err == nil || len(results) != 2 || results[1].ExitCode != 1 {
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
	results, err := (LocalRunner{}).Run(context.Background(), t.TempDir(), []Command{{Command: "sleep 1", Timeout: 10 * time.Millisecond}})
	if err == nil || len(results) != 1 || !results[0].TimedOut || results[0].ExitCode != -1 {
		t.Fatalf("results = %#v, error = %v", results, err)
	}
}
