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

func TestValidateLedgerAcceptsBaselinedDatabase(t *testing.T) {
	if err := validateLedger(18, false, 18); err != nil {
		t.Fatalf("validateLedger rejected a freshly baselined database: %v", err)
	}
}

// A database baselined at the legacy maximum keeps that legacy row forever, so
// every migration added after the Go rewrite leaves golang-migrate ahead of it.
// Restarting such a database must keep working.
func TestValidateLedgerAcceptsMigrationsAddedAfterBaseline(t *testing.T) {
	for _, current := range []uint{19, 20, 50} {
		if err := validateLedger(current, false, 18); err != nil {
			t.Fatalf("validateLedger(%d, false, 18) = %v; a legacy database cannot restart once new migrations land", current, err)
		}
	}
}

func TestValidateLedgerRejectsLedgerBehindLegacySchema(t *testing.T) {
	if err := validateLedger(17, false, 18); err == nil {
		t.Fatal("validateLedger accepted a golang-migrate version behind the legacy ledger")
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
