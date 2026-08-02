package toolchain

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validManifest = `{
  "schemaVersion": "1.0",
  "image": "test-image",
  "summary": "An image used by the tests.",
  "tools": [{"name": "git", "purpose": "Version control."}],
  "absent": [{"name": "python3", "note": "No Python runtime."}],
  "notes": ["Trust this list."]
}`

func writeManifest(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolchain.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestLoadReadsAndValidatesAManifest(t *testing.T) {
	manifest, err := Load(writeManifest(t, validManifest))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if manifest.Image != "test-image" || len(manifest.Tools) != 1 || len(manifest.Absent) != 1 {
		t.Fatalf("loaded manifest = %#v", manifest)
	}
}

// A missing manifest is reported as its own condition so the runner can tell an
// environment with nothing to say from one whose declaration is broken. The
// first is normal and silent; the second is a defect worth a warning.
func TestLoadReportsAMissingManifestDistinctly(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("Load(missing) error = %v, want ErrNoManifest", err)
	}
}

func TestLoadRejectsAnOversizedManifest(t *testing.T) {
	path := writeManifest(t, validManifest+strings.Repeat(" ", maxManifestBytes))
	if _, err := Load(path); err == nil || errors.Is(err, ErrNoManifest) {
		t.Fatalf("Load(oversized) error = %v", err)
	}
}

func TestParseRejectsInvalidManifests(t *testing.T) {
	for _, testCase := range []struct{ name, contents string }{
		{"wrong schema", `{"schemaVersion":"2.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"}]}`},
		{"no image", `{"schemaVersion":"1.0","image":"","summary":"s","tools":[{"name":"git","purpose":"p"}]}`},
		{"no summary", `{"schemaVersion":"1.0","image":"i","summary":"","tools":[{"name":"git","purpose":"p"}]}`},
		{"no tools", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[]}`},
		{"blank purpose", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":""}]}`},
		{"bad name", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"../git","purpose":"p"}]}`},
		{"blank absence note", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"}],"absent":[{"name":"go","note":""}]}`},
		// A tool that is both present and absent puts a contradiction in front
		// of the agent, which is worse than saying nothing at all.
		{"contradiction", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"}],"absent":[{"name":"git","note":"n"}]}`},
		{"duplicate tool", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"},{"name":"git","purpose":"q"}]}`},
		// A field this runner does not understand is a declaration it would
		// drop while still telling the agent the list is complete.
		{"unknown field", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"}],"runtimes":[]}`},
		{"trailing value", `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"git","purpose":"p"}]}{}`},
		// Terminal escapes have no place in prose that is spliced into a prompt.
		// Written as the JSON escape a manifest would carry them in, so this
		// exercises the text check rather than the JSON decoder.
		{"escape sequence", `{"schemaVersion":"1.0","image":"i","summary":"s\u001b[31m","tools":[{"name":"git","purpose":"p"}]}`},
		{"not json", `nonsense`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := Parse([]byte(testCase.contents)); err == nil {
				t.Fatalf("Parse(%s) accepted an invalid manifest", testCase.name)
			}
		})
	}
}

func TestVerifyFailsOnADeclaredToolThatIsNotInstalled(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	err = manifest.Verify(func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "declared but not installed: git") {
		t.Fatalf("Verify(nothing installed) error = %v", err)
	}
}

// The rot direction: someone installs python3 and the declaration still tells
// the agent it is not there, talking it out of a tool it has.
func TestVerifyFailsOnADeclaredAbsenceThatIsInstalled(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	err = manifest.Verify(func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "declared absent but installed: python3") {
		t.Fatalf("Verify(everything installed) error = %v", err)
	}
}

func TestVerifyAcceptsAMatchingEnvironment(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := manifest.Verify(func(name string) bool { return name == "git" }); err != nil {
		t.Fatalf("Verify(matching) error = %v", err)
	}
}

func TestVerifyRequiresALookup(t *testing.T) {
	if err := (Manifest{}).Verify(nil); err == nil {
		t.Fatal("Verify(nil) accepted a missing lookup")
	}
}

func TestLookupInFindsOnlyExecutableFilesOnTheGivenPath(t *testing.T) {
	present := t.TempDir()
	absent := t.TempDir()
	if err := os.WriteFile(filepath.Join(present, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write tool: %v", err)
	}
	if err := os.WriteFile(filepath.Join(present, "data"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := os.Mkdir(filepath.Join(present, "directory"), 0o755); err != nil {
		t.Fatalf("make directory: %v", err)
	}
	path := strings.Join([]string{absent, present}, string(os.PathListSeparator))
	for name, want := range map[string]bool{"tool": true, "data": false, "directory": false, "missing": false, "": false, "sub/tool": false} {
		if got := LookupIn(path, name); got != want {
			t.Fatalf("LookupIn(%q) = %t, want %t", name, got, want)
		}
	}
}

// The declaration is what the agent reads, so both halves have to survive
// rendering: what is installed, and what deliberately is not.
func TestDeclarationRendersToolsAbsencesAndNotes(t *testing.T) {
	manifest, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	declaration := manifest.Declaration()
	for _, want := range []string{"test-image", "An image used by the tests.", "Available:", "`git`", "Version control.", "Not installed:", "`python3`", "No Python runtime.", "Trust this list."} {
		if !strings.Contains(declaration, want) {
			t.Fatalf("Declaration() = %q, missing %q", declaration, want)
		}
	}
}

// The manifest the runner image actually ships. It is verified against the
// image at build time, but a syntax or schema mistake should fail here first,
// in a second, rather than in a five-minute Docker build.
func TestShippedRunnerManifestIsValid(t *testing.T) {
	manifest, err := Load(filepath.Join("..", "..", "toolchain.json"))
	if err != nil {
		t.Fatalf("Load(runner/toolchain.json) error = %v", err)
	}
	if manifest.Image != "moirai-runner" {
		t.Fatalf("shipped manifest image = %q", manifest.Image)
	}
	declared := make(map[string]struct{}, len(manifest.Absent))
	for _, absent := range manifest.Absent {
		declared[absent.Name] = struct{}{}
	}
	// The absences that cost a real run an attempt. python3 is the one from the
	// field report; make and go are the next two an agent working a Go
	// repository with a Makefile reaches for.
	for _, name := range []string{"python3", "make", "go", "docker"} {
		if _, ok := declared[name]; !ok {
			t.Fatalf("shipped manifest does not declare %q absent", name)
		}
	}
}
