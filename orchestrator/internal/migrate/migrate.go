package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var migrationName = regexp.MustCompile(`^(\d+)_.+\.sql$`)

type sourceFS struct{ fs.FS }

type sourceEntry struct {
	fs.DirEntry
	name string
}

func (entry sourceEntry) Name() string { return entry.name }

func (source sourceFS) Open(name string) (fs.File, error) {
	return source.FS.Open(strings.Replace(name, ".up.sql", ".sql", 1))
}

func (source sourceFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(source.FS, name)
	if err != nil {
		return nil, err
	}
	result := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !migrationName.MatchString(entry.Name()) {
			continue
		}
		result = append(result, sourceEntry{DirEntry: entry, name: strings.TrimSuffix(entry.Name(), ".sql") + ".up.sql"})
	}
	return result, nil
}

func Apply(ctx context.Context, databaseURL string, source fs.FS) error {
	if databaseURL == "" {
		return errors.New("database URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect migration database: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS app`); err != nil {
		return fmt.Errorf("create migration schema: %w", err)
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{MigrationsTable: `"app"."schema_migrations"`, MigrationsTableQuoted: true})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}
	migrationSource, err := iofs.New(sourceFS{source}, "migrations")
	if err != nil {
		return fmt.Errorf("load migration source: %w", err)
	}
	engine, err := migrate.NewWithInstance("iofs", migrationSource, "postgres", driver)
	if err != nil {
		return fmt.Errorf("create migration engine: %w", err)
	}
	defer engine.Close()
	if err := baselineLegacy(ctx, db, engine); err != nil {
		return err
	}
	if err := engine.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

func baselineLegacy(ctx context.Context, db *sql.DB, engine *migrate.Migrate) error {
	var legacy bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('app.schema_version') IS NOT NULL`).Scan(&legacy); err != nil {
		return fmt.Errorf("check legacy migration ledger: %w", err)
	}
	if !legacy {
		return nil
	}
	var version, minimum sql.NullInt64
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT MIN(version), MAX(version), COUNT(*) FROM app.schema_version`).Scan(&minimum, &version, &count); err != nil {
		return fmt.Errorf("read legacy migration ledger: %w", err)
	}
	if !version.Valid {
		return nil
	}
	if !minimum.Valid || minimum.Int64 != 1 || count != version.Int64 {
		return errors.New("legacy migration ledger is not contiguous")
	}
	current, dirty, err := engine.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("read golang-migrate version: %w", err)
	}
	if errors.Is(err, migrate.ErrNilVersion) {
		if err := engine.Force(int(version.Int64)); err != nil {
			return fmt.Errorf("baseline legacy migration ledger: %w", err)
		}
		return nil
	}
	if err := validateLedger(current, dirty, version.Int64); err != nil {
		return err
	}
	return nil
}

// validateLedger checks a database that still carries the legacy
// app.schema_version table after golang-migrate has taken ownership.
//
// The legacy ledger is a baseline, not a running total. It is frozen at
// whatever version the Python migrator last applied, while golang-migrate keeps
// advancing past it with every migration added from now on. Requiring the two
// to be equal would therefore boot fine today and fail permanently the day
// migration 019 lands, because the legacy table is never dropped. Only a
// golang-migrate version *behind* the legacy ledger is a genuine disagreement:
// that would replay SQL the database demonstrably already has.
func validateLedger(current uint, dirty bool, legacy int64) error {
	if dirty {
		return errors.New("golang-migrate migration is dirty; resolve it before restarting")
	}
	if legacy > 0 && current < uint(legacy) {
		return fmt.Errorf("golang-migrate is at version %d but the legacy ledger reached %d", current, legacy)
	}
	return nil
}
