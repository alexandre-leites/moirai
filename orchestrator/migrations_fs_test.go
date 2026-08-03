package orchestrator

import (
	"io/fs"
	"os"
	"testing"
)

// TestMigrationsSurviveWorkingDirectoryChange proves the embedded migrations
// are compiled into the binary rather than read off disk: previously
// migrate.Apply(ctx, url, os.DirFS(".")) only worked because the process
// happened to be started from a directory containing a migrations/ folder.
// Changing into an unrelated directory here simulates any other invocation
// (go run from the repo root, a systemd unit with no WorkingDirectory) and
// must not affect what Migrations contains.
func TestMigrationsSurviveWorkingDirectoryChange(t *testing.T) {
	before, err := fs.ReadDir(Migrations, "migrations")
	if err != nil || len(before) == 0 {
		t.Fatalf("embedded migrations unreadable before chdir: entries=%d err=%v", len(before), err)
	}

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Fatal(err)
		}
	})

	after, err := fs.ReadDir(Migrations, "migrations")
	if err != nil {
		t.Fatalf("embedded migrations unreadable from an unrelated working directory: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("entry count changed after chdir: before=%d after=%d", len(before), len(after))
	}
}
