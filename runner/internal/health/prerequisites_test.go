package health

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckRequiresGitAndOpenCodeAndWritableDataDirectory(t *testing.T) {
	dataDirectory := t.TempDir()
	lookedUp := make([]string, 0, 2)
	dependencies := Dependencies{
		LookPath: func(binary string) (string, error) {
			lookedUp = append(lookedUp, binary)
			return "/bin/" + binary, nil
		},
		MkdirAll: os.MkdirAll,
		Create:   os.Create,
		Remove:   os.Remove,
	}
	if err := Check(dataDirectory, false, dependencies); err != nil {
		t.Fatal(err)
	}
	if len(lookedUp) != 2 || lookedUp[0] != "git" || lookedUp[1] != "opencode" {
		t.Fatalf("unexpected prerequisites: %v", lookedUp)
	}
	if _, err := os.Stat(filepath.Join(dataDirectory, ".loop-write-probe")); !os.IsNotExist(err) {
		t.Fatalf("probe file was not removed: %v", err)
	}
}

func TestCheckRequiresDockerWhenConfiguredAndFailsClosed(t *testing.T) {
	dependencies := Dependencies{
		LookPath: func(binary string) (string, error) {
			if binary == "docker" {
				return "", errors.New("not installed")
			}
			return "/bin/" + binary, nil
		},
		MkdirAll: os.MkdirAll,
		Create:   os.Create,
		Remove:   os.Remove,
	}
	if err := Check(t.TempDir(), true, dependencies); err == nil {
		t.Fatal("expected missing Docker failure")
	}
	if err := Check(t.TempDir(), false, Dependencies{}); err == nil {
		t.Fatal("expected invalid dependency failure")
	}
}
