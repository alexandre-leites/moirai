package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityStoreSaveAndLoadUsesPrivateAtomicFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "runner.json")
	store := IdentityStore{Path: path}
	want := Identity{RunnerID: "runner-1", Credential: "credential-1"}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity permissions = %o, want 600", got)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestLoadOrRegisterReusesStoredIdentityOrRegistersOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "runner.json")
	store := IdentityStore{Path: path}
	service := &fakeService{stream: &fakeStream{}}
	identity, err := LoadOrRegister(context.Background(), store, service, "token", "runner", []string{"docker"}, 2)
	if err != nil {
		t.Fatalf("LoadOrRegister() registration error = %v", err)
	}
	if identity.RunnerID != "runner-1" || service.registration == nil {
		t.Fatalf("LoadOrRegister() registered identity = %#v, request = %#v", identity, service.registration)
	}
	if service.registration.GetCapacity() != 2 {
		t.Fatalf("LoadOrRegister() capacity = %d, want 2", service.registration.GetCapacity())
	}
	service.registration = nil
	loaded, err := LoadOrRegister(context.Background(), store, service, "", "", nil, 1)
	if err != nil {
		t.Fatalf("LoadOrRegister() stored identity error = %v", err)
	}
	if loaded != identity || service.registration != nil {
		t.Fatalf("LoadOrRegister() loaded = %#v, registration = %#v", loaded, service.registration)
	}
}

func TestLoadOrRegisterRequiresTokenForMissingIdentity(t *testing.T) {
	store := IdentityStore{Path: filepath.Join(t.TempDir(), "runner.json")}
	if _, err := LoadOrRegister(context.Background(), store, &fakeService{}, "", "runner", nil, 1); err == nil {
		t.Fatal("LoadOrRegister() accepted a missing identity without registration token")
	}
}

func TestIdentityStoreRejectsUnsafeOrInvalidIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.json")
	store := IdentityStore{Path: path}
	if err := store.Save(Identity{}); err == nil {
		t.Fatal("Save() accepted an empty identity")
	}
	if err := os.WriteFile(path, []byte(`{"RunnerID":"runner-1","Credential":"credential-1"}`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted group-readable identity")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load() accepted invalid JSON")
	}
	if _, err := (IdentityStore{}).Load(); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() empty path error = %v", err)
	}
}
