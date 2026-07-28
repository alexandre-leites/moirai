package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileManifestsRemovesStaleExecutionRecords(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaces", "job-1", "repository", ".loop", "execution-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"backend":"cli","executionId":"execution-1","pid":999999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileManifests(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("manifest remains: %v", err)
	}
}

func TestReconcileManifestsSkipsInvalidRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaces", "job-1", "repository", ".loop", "execution-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileManifests(root); err != nil {
		t.Fatalf("ReconcileManifests() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid manifest remains: %v", err)
	}
}
