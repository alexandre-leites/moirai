package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
)

func TestLogTailPrefersTheFailingPipelineCommandOutput(t *testing.T) {
	workspace := testWorkspace(t)
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Loop, "opencode.stderr.log"), []byte("agent noise\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := []pipeline.Result{
		{Command: "go build ./...", ExitCode: 0, Output: "ok"},
		{Command: "go test ./...", ExitCode: 1, Output: "--- FAIL: TestThing\n"},
	}
	if got := logTail(workspace, results); got != "--- FAIL: TestThing" {
		t.Fatalf("logTail() = %q", got)
	}
}

func TestLogTailFallsBackToAgentLogsPreferringStandardError(t *testing.T) {
	workspace := testWorkspace(t)
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Loop, "opencode.stdout.log"), []byte("progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(workspace, nil); got != "progress" {
		t.Fatalf("logTail() with only stdout = %q", got)
	}
	if err := os.WriteFile(filepath.Join(workspace.Loop, "opencode.stderr.log"), []byte("panic: nil map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(workspace, nil); got != "panic: nil map" {
		t.Fatalf("logTail() with stderr = %q", got)
	}
}

func TestLogTailIsEmptyWithoutAnyEvidence(t *testing.T) {
	workspace := testWorkspace(t)
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Loop, "opencode.stderr.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(workspace, []pipeline.Result{{Command: "go test", ExitCode: 0, Output: "ok"}}); got != "" {
		t.Fatalf("logTail() = %q, want empty", got)
	}
	if got := logTail(repository.Workspace{}, nil); got != "" {
		t.Fatalf("logTail() without a workspace = %q", got)
	}
}

// TestLogTailBoundsAndSanitisesUnboundedOutput guards the failure mode that
// lost terminal events before: an unbounded string field. The bound is on the
// ENCODED size, so the worst case is not an escape sequence (those are stripped)
// but ordinary text full of the bytes Go's JSON encoder expands sixfold.
func TestLogTailBoundsAndSanitisesUnboundedOutput(t *testing.T) {
	tests := map[string]string{
		"ansi and control bytes": strings.Repeat("\x1b[31mERROR\x1b[0m line with \x00 junk\n", 4096),
		"html significant bytes": strings.Repeat("File \"x.py\", line 1, in <module> a & b > c\n", 4096),
		"quotes and backslashes": strings.Repeat(`expected "a\b" got "c\d" `, 4096),
		"multi-byte runes":       strings.Repeat("erreur: identifiant inconnu — café ☕\n", 4096),
	}
	for name, noisy := range tests {
		t.Run(name, func(t *testing.T) {
			tail := boundedLogTail(noisy)
			if strings.ContainsAny(tail, "\x1b\x00") {
				t.Fatalf("log tail kept control characters: %q", tail)
			}
			if !utf8.ValidString(tail) {
				t.Fatalf("log tail is not valid UTF-8: %q", tail)
			}
			if !strings.HasPrefix(tail, truncationMarker) {
				t.Fatalf("truncated log tail is not marked as truncated: %q", tail[:min(len(tail), 32)])
			}
			encoded, err := json.Marshal(tail)
			if err != nil {
				t.Fatal(err)
			}
			// json.Marshal wraps the value in two quote characters.
			if len(encoded)-2 > maxLogTailBytes {
				t.Fatalf("log tail encodes to %d bytes, want at most %d (raw length %d)", len(encoded)-2, maxLogTailBytes, len(tail))
			}
		})
	}
	if got := boundedLogTail("\x1b[31mERROR\x1b[0m line with \x00 junk"); got != "ERROR line with  junk" {
		t.Fatalf("sanitised log tail = %q", got)
	}
}

// TestJSONEncodedSizeMatchesTheEncoder keeps the budget honest: a wrong cost
// model would silently let the tail exceed the payload limit again.
func TestJSONEncodedSizeMatchesTheEncoder(t *testing.T) {
	for _, value := range []string{"", "plain text", `quotes " and \ backslash`, "html < > & significant", "tab\tnewline\n", "café ☕ 界", "mixed <a href=\"x\">\tné\n"} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jsonEncodedSize(value), len(encoded)-2; got != want {
			t.Fatalf("jsonEncodedSize(%q) = %d, want %d (%s)", value, got, want, encoded)
		}
	}
}

func TestBoundedLogTailKeepsRunesIntactAndDropsInvalidBytes(t *testing.T) {
	if got := boundedLogTail("caf\xc3\xa9 \xff done"); got != "café  done" {
		t.Fatalf("boundedLogTail() = %q", got)
	}
	tail := boundedLogTail(strings.Repeat("é", maxLogTailBytes))
	if !utf8.ValidString(tail) || len(tail) > maxLogTailBytes {
		t.Fatalf("boundedLogTail() = %q (%d bytes)", tail, len(tail))
	}
}

func TestFileTailReadsOnlyTheEndOfALargeLog(t *testing.T) {
	workspace := testWorkspace(t)
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace.Loop, "cli.stderr.log")
	contents := strings.Repeat("noise\n", 100_000) + "final failure line"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := fileTail(path)
	if !strings.HasSuffix(tail, "final failure line") || len(tail) > maxLogTailBytes {
		t.Fatalf("fileTail() = %q (%d bytes)", tail, len(tail))
	}
}
