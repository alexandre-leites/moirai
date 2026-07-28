package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
			slog.Warn("skip unreadable stale execution manifest", "path", path, "error", err)
			continue
		}
		var manifest executionManifest
		if err := json.Unmarshal(contents, &manifest); err != nil || manifest.ExecutionID == "" || manifest.PID < 1 {
			slog.Warn("discard invalid stale execution manifest", "path", path)
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				slog.Warn("remove invalid stale execution manifest", "path", path, "error", removeErr)
			}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove stale execution manifest", "path", path, "error", err)
		}
	}
	return nil
}
