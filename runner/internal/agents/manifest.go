package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func writeExecutionManifest(directory, backend, jobID string, generation int64, executionID string, pid int) {
	started, err := processStartTime(pid)
	if err != nil || jobID == "" || generation < 1 || executionID == "" {
		return
	}
	contents, err := json.Marshal(executionManifest{
		Backend:          backend,
		JobID:            jobID,
		LeaseGeneration:  generation,
		ExecutionID:      executionID,
		PID:              pid,
		ProcessStartTime: started,
	})
	if err == nil {
		_ = os.WriteFile(filepath.Join(directory, "execution-manifest.json"), append(contents, '\n'), 0o600)
	}
}
