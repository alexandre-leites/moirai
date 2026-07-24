package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func writeExecutionManifest(directory, backend, executionID string, pid int) {
	contents, err := json.Marshal(struct {
		Backend     string    `json:"backend"`
		ExecutionID string    `json:"executionId"`
		PID         int       `json:"pid"`
		StartedAt   time.Time `json:"startedAt"`
	}{backend, executionID, pid, time.Now().UTC()})
	if err == nil {
		_ = os.WriteFile(filepath.Join(directory, "execution-manifest.json"), append(contents, '\n'), 0o600)
	}
}
