package migrate

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	orchestrator "github.com/loop-engineering/orchestrator"
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

// TestSourceFSRejectsMisnamedSQLFiles guards against #270-style outages: a
// migration file that almost matches the naming convention used to be
// skipped with no warning and no count, so the orchestrator started
// "successfully" against a schema missing whatever that file would have
// created. Any of these near-misses must now fail startup loudly, naming the
// offending file, instead of silently vanishing from the migration set.
func TestSourceFSRejectsMisnamedSQLFiles(t *testing.T) {
	cases := map[string]string{
		"hyphen instead of underscore": "019-add-foo.sql",
		"upper-cased extension":        "019_add_foo.SQL",
		"missing leading version":      "add_foo.sql",
	}
	for name, filename := range cases {
		t.Run(name, func(t *testing.T) {
			source := sourceFS{fstest.MapFS{
				"migrations/001_first.sql": {Data: []byte("SELECT 1;")},
				"migrations/" + filename:   {Data: []byte("SELECT 2;")},
			}}
			_, err := fs.ReadDir(source, "migrations")
			if err == nil {
				t.Fatalf("ReadDir accepted mis-named migration file %q", filename)
			}
			if !strings.Contains(err.Error(), filename) {
				t.Fatalf("error %q does not name the offending file %q", err.Error(), filename)
			}
		})
	}
}

// TestRealMigrationsDirectoryNamesAreValid is a regression test against the
// actual embedded migrations shipped with the binary: every file the build
// bakes in via go:embed must satisfy the naming pattern, or the orchestrator
// would fail to start in production.
func TestRealMigrationsDirectoryNamesAreValid(t *testing.T) {
	source := sourceFS{orchestrator.Migrations}
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		t.Fatalf("real embedded migrations directory failed validation: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one embedded migration file")
	}
}
