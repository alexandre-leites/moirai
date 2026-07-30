package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReconcileManifestsTerminatesMatchingProcessAndReturnsFencedExecution(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaces", "job-1", "repository", ".loop", "execution-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"backend":"cli","jobId":"job-1","leaseGeneration":2,"executionId":"execution-1","pid":42,"processStartTime":"100"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	terminated := 0
	recovered, err := reconcileManifests(root, func(pid int) (string, error) {
		if pid != 42 {
			t.Fatalf("pid = %d", pid)
		}
		return "100", nil
	}, func(pid int) error {
		terminated = pid
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminated != 42 {
		t.Fatalf("terminated pid = %d", terminated)
	}
	if len(recovered) != 1 || recovered[0] != (RecoveredExecution{JobID: "job-1", LeaseGeneration: 2, ExecutionID: "execution-1"}) {
		t.Fatalf("recovered = %#v", recovered)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("manifest remains: %v", err)
	}
}

func TestReconcileManifestsDoesNotTerminateChangedProcess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspaces", "job-1", "repository", ".loop", "execution-manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"backend":"cli","jobId":"job-1","leaseGeneration":2,"executionId":"execution-1","pid":42,"processStartTime":"100"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := reconcileManifests(root, func(int) (string, error) { return "101", nil }, func(int) error {
		t.Fatal("terminated process with changed identity")
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	recovered, err := ReconcileManifests(root)
	if err != nil {
		t.Fatalf("ReconcileManifests() error = %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("recovered = %#v", recovered)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid manifest remains: %v", err)
	}
}
