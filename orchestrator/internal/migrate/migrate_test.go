package migrate

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestSourceFSExposesLegacyFilesAsUpMigrations(t *testing.T) {
	source := sourceFS{fstest.MapFS{
		"migrations/010_second.sql": {Data: []byte("SELECT 2;")},
		"migrations/001_first.sql":  {Data: []byte("SELECT 1;")},
	}}
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "001_first.up.sql" || entries[1].Name() != "010_second.up.sql" {
		t.Fatalf("entries = %#v", entries)
	}
	contents, err := fs.ReadFile(source, "migrations/001_first.up.sql")
	if err != nil || string(contents) != "SELECT 1;" {
		t.Fatalf("contents = %q, %v", contents, err)
	}
}

func TestValidateLedgerReportsDirtyMigration(t *testing.T) {
	err := validateLedger(17, true, 18)
	if err == nil || err.Error() != "golang-migrate migration is dirty; resolve it before restarting" {
		t.Fatalf("error = %v", err)
	}
}

func TestSourceFSSkipsNonMigrationFiles(t *testing.T) {
	source := sourceFS{fstest.MapFS{
		"migrations/001_first.sql": {Data: []byte("SELECT 1;")},
		"migrations/notes.txt":     {Data: []byte("ignored")},
	}}
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "001_first.up.sql" {
		t.Fatalf("entries = %#v", entries)
	}
}
