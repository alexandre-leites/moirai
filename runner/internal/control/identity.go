package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type IdentityStore struct {
	Path string
}

func LoadOrRegister(ctx context.Context, store IdentityStore, service ControlService, token, name string, labels []string, capacity int) (Identity, error) {
	identity, err := store.Load()
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Identity{}, err
	}
	if token == "" {
		return Identity{}, errors.New("runner registration token is required when no stored identity exists")
	}
	identity, err = Register(ctx, service, token, name, labels, capacity)
	if err != nil {
		return Identity{}, fmt.Errorf("register runner: %w", err)
	}
	if err := store.Save(identity); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func (store IdentityStore) Load() (Identity, error) {
	path, err := store.path()
	if err != nil {
		return Identity{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Identity{}, fmt.Errorf("read runner identity: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Identity{}, errors.New("runner identity must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Identity{}, errors.New("runner identity permissions must not allow group or other access")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("read runner identity: %w", err)
	}
	var identity Identity
	if err := json.Unmarshal(contents, &identity); err != nil {
		return Identity{}, fmt.Errorf("parse runner identity: %w", err)
	}
	if identity.RunnerID == "" || identity.Credential == "" {
		return Identity{}, errors.New("runner identity is invalid")
	}
	return identity, nil
}

func (store IdentityStore) Save(identity Identity) error {
	if identity.RunnerID == "" || identity.Credential == "" {
		return errors.New("runner identity and credential are required")
	}
	path, err := store.path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runner identity directory: %w", err)
	}
	contents, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode runner identity: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".runner-identity-*")
	if err != nil {
		return fmt.Errorf("create runner identity temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure runner identity temporary file: %w", err)
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write runner identity: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync runner identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close runner identity: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace runner identity: %w", err)
	}
	return nil
}

func (store IdentityStore) path() (string, error) {
	if store.Path == "" {
		return "", errors.New("runner identity path is required")
	}
	return filepath.Abs(filepath.Clean(store.Path))
}
