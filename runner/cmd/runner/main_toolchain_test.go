package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const declaredToolchain = `{
  "schemaVersion": "1.0",
  "image": "moirai-runner",
  "summary": "The image under test.",
  "tools": [{"name": "sh", "purpose": "POSIX shell."}],
  "absent": [{"name": "moirai-absent-tool", "note": "Deliberately not installed."}],
  "notes": ["Trust this list."]
}`

func manifestAt(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "toolchain.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestToolchainCommandOnlyClaimsItsOwnArguments(t *testing.T) {
	for _, arguments := range [][]string{nil, {"live"}, {"ready"}} {
		if handled, err := toolchainCommand(arguments, "", io.Discard); handled || err != nil {
			t.Fatalf("toolchainCommand(%v) = (%t, %v)", arguments, handled, err)
		}
	}
	handled, err := toolchainCommand([]string{"toolchain", "--nonsense"}, "", io.Discard)
	if !handled || err == nil {
		t.Fatalf("toolchainCommand(--nonsense) = (%t, %v)", handled, err)
	}
}

func TestToolchainCommandPrintsTheDeclaration(t *testing.T) {
	var out strings.Builder
	handled, err := toolchainCommand([]string{"toolchain"}, manifestAt(t, declaredToolchain), &out)
	if !handled || err != nil {
		t.Fatalf("toolchainCommand(toolchain) = (%t, %v)", handled, err)
	}
	for _, want := range []string{"moirai-runner", "`sh`", "Not installed:", "`moirai-absent-tool`"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("declaration is missing %q:\n%s", want, out.String())
		}
	}
}

// The check the image build and CI both run. `sh` is on the agent's PATH in
// every environment this test can run in, and a tool named for this repository
// is on nobody's, so the assertion is real rather than tautological.
func TestToolchainVerifyAcceptsAManifestThatMatchesTheEnvironment(t *testing.T) {
	handled, err := toolchainCommand([]string{"toolchain", "--verify"}, manifestAt(t, declaredToolchain), io.Discard)
	if !handled || err != nil {
		t.Fatalf("toolchainCommand(--verify) = (%t, %v)", handled, err)
	}
}

func TestToolchainVerifyRejectsAManifestThatDoesNotMatchTheEnvironment(t *testing.T) {
	for name, contents := range map[string]string{
		"declared but not installed":  `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"moirai-absent-tool","purpose":"p"}]}`,
		"declared absent but present": `{"schemaVersion":"1.0","image":"i","summary":"s","tools":[{"name":"sh","purpose":"p"}],"absent":[{"name":"env","note":"n"}]}`,
		"unreadable":                  `{"schemaVersion":"9.9"}`,
	} {
		t.Run(name, func(t *testing.T) {
			handled, err := toolchainCommand([]string{"toolchain", "--verify"}, manifestAt(t, contents), io.Discard)
			if !handled || err == nil {
				t.Fatalf("toolchainCommand(--verify) = (%t, %v)", handled, err)
			}
		})
	}
}

func TestToolchainCommandFailsWhenTheImageDeclaresNothing(t *testing.T) {
	handled, err := toolchainCommand([]string{"toolchain"}, filepath.Join(t.TempDir(), "absent.json"), io.Discard)
	if !handled || err == nil {
		t.Fatalf("toolchainCommand(missing manifest) = (%t, %v)", handled, err)
	}
}
