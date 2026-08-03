package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalFileTaskSourceListsTasks is the seam's proof: a TaskSource
// implementation that never touches GitHub (or the network) end to end for
// ListTasks, demonstrating a project's issue_tracker_type can genuinely swap
// out the tracker rather than secretly always going through the GitHub
// adapter.
func TestLocalFileTaskSourceListsTasks(t *testing.T) {
	dir := t.TempDir()
	writeTaskFile(t, dir, "ready.json", `{"title":"Fix scheduler","body":"details","url":"file://ready","priority":5,"eligible":true}`)
	writeTaskFile(t, dir, "blocked.json", `{"title":"Not ready","eligible":false}`)
	writeTaskFile(t, dir, "closed.json", `{"title":"Already done","eligible":true,"state":"closed"}`)
	writeTaskFile(t, dir, "custom-id.json", `{"external_id":"CUSTOM-1","title":"Explicit ID","eligible":true}`)
	// A non-JSON file in the same directory must be ignored rather than
	// breaking the whole listing.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a task"), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks, err := LocalFileTaskSource{}.ListTasks(context.Background(), "p1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("len(tasks) = %d, want 4: %#v", len(tasks), tasks)
	}

	byID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		byID[task.ExternalID] = task
	}

	ready, ok := byID["ready"]
	if !ok {
		t.Fatal("missing task \"ready\"")
	}
	if !ready.Eligible || ready.Priority != 5 || ready.State != "open" || ready.Body != "details" || ready.URL != "file://ready" {
		t.Fatalf("ready = %#v", ready)
	}
	if ready.Raw == nil {
		t.Fatal("ready.Raw was not populated")
	}

	blocked, ok := byID["blocked"]
	if !ok || blocked.Eligible {
		t.Fatalf("blocked = %#v, ok=%v", blocked, ok)
	}

	closed, ok := byID["closed"]
	if !ok || closed.Eligible || closed.State != "closed" {
		t.Fatalf("closed = %#v, ok=%v", closed, ok)
	}

	custom, ok := byID["CUSTOM-1"]
	if !ok || custom.Title != "Explicit ID" {
		t.Fatalf("CUSTOM-1 = %#v, ok=%v", custom, ok)
	}
}

func TestLocalFileTaskSourceRejectsMissingDirectory(t *testing.T) {
	if _, err := (LocalFileTaskSource{}).ListTasks(context.Background(), "p1", filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestLocalFileTaskSourceRejectsEmptyRef(t *testing.T) {
	if _, err := (LocalFileTaskSource{}).ListTasks(context.Background(), "p1", ""); err == nil {
		t.Fatal("expected an error for an empty ref")
	}
}

func TestLocalFileTaskSourceRejectsATaskWithNoTitle(t *testing.T) {
	dir := t.TempDir()
	writeTaskFile(t, dir, "no-title.json", `{"eligible":true}`)
	if _, err := (LocalFileTaskSource{}).ListTasks(context.Background(), "p1", dir); err == nil {
		t.Fatal("expected an error for a task file with no title")
	}
}

// TestLocalFileTaskSourceSatisfiesTaskSource pins the seam itself: a value of
// this type is assignable wherever a TaskSource is required, with no adapter
// shim needed.
func TestLocalFileTaskSourceSatisfiesTaskSource(t *testing.T) {
	var _ TaskSource = LocalFileTaskSource{}
}

func writeTaskFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
