package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type executionManifest struct {
	Backend     string `json:"backend"`
	ExecutionID string `json:"executionId"`
	PID         int    `json:"pid"`
}

func ReconcileManifests(dataDirectory string) error {
	if dataDirectory == "" || !filepath.IsAbs(dataDirectory) {
		return errors.New("runner data directory is invalid")
	}
	paths, err := filepath.Glob(filepath.Join(dataDirectory, "workspaces", "job-*", "repository", ".loop", "execution-manifest.json"))
	if err != nil {
		return fmt.Errorf("list execution manifests: %w", err)
	}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read execution manifest: %w", err)
		}
		var manifest executionManifest
		if err := json.Unmarshal(contents, &manifest); err != nil || manifest.ExecutionID == "" || manifest.PID < 1 {
			return errors.New("execution manifest is invalid")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove execution manifest: %w", err)
		}
	}
	return nil
}
