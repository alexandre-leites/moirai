package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadNormalizesPythonDatabaseURL(t *testing.T) {
	t.Setenv("LOOP_DATABASE_URL", "postgresql+asyncpg://loop:secret@postgres:5432/loop")
	t.Setenv("LOOP_GRPC_BIND", "127.0.0.1:50051")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseURL != "postgresql://loop:secret@postgres:5432/loop" {
		t.Fatalf("DatabaseURL = %q", got.DatabaseURL)
	}
}

func TestLoadReadsSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte("postgres://loop:secret@postgres/loop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_DATABASE_URL", "")
	t.Setenv("LOOP_DATABASE_URL_FILE", path)
	t.Setenv("LOOP_GRPC_BIND", "127.0.0.1:50051")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseURL != "postgres://loop:secret@postgres/loop" {
		t.Fatalf("DatabaseURL = %q", got.DatabaseURL)
	}
}

func TestLoadRejectsAmbiguousSecret(t *testing.T) {
	t.Setenv("LOOP_DATABASE_URL", "postgres://loop:secret@postgres/loop")
	t.Setenv("LOOP_DATABASE_URL_FILE", "database-url")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted both database URL sources")
	}
}
