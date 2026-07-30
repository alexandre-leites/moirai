package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type executionManifest struct {
	Backend          string `json:"backend"`
	JobID            string `json:"jobId"`
	LeaseGeneration  int64  `json:"leaseGeneration"`
	ExecutionID      string `json:"executionId"`
	PID              int    `json:"pid"`
	ProcessStartTime string `json:"processStartTime"`
}

type RecoveredExecution struct {
	JobID           string
	LeaseGeneration int64
	ExecutionID     string
}

func ReconcileManifests(dataDirectory string) ([]RecoveredExecution, error) {
	return reconcileManifests(dataDirectory, processStartTime, terminateProcessGroup)
}

func reconcileManifests(dataDirectory string, startTime func(int) (string, error), terminate func(int) error) ([]RecoveredExecution, error) {
	if dataDirectory == "" || !filepath.IsAbs(dataDirectory) {
		return nil, errors.New("runner data directory is invalid")
	}
	paths, err := filepath.Glob(filepath.Join(dataDirectory, "workspaces", "job-*", "repository", ".loop", "execution-manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("list execution manifests: %w", err)
	}
	recovered := make([]RecoveredExecution, 0, len(paths))
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("skip unreadable stale execution manifest", "path", path, "error", err)
			continue
		}
		var manifest executionManifest
		if err := json.Unmarshal(contents, &manifest); err != nil || manifest.JobID == "" || manifest.LeaseGeneration < 1 || manifest.ExecutionID == "" || manifest.PID < 1 || manifest.ProcessStartTime == "" {
			slog.Warn("discard invalid stale execution manifest", "path", path)
			removeManifest(path)
			continue
		}
		if actual, err := startTime(manifest.PID); err == nil && actual == manifest.ProcessStartTime {
			if err := terminate(manifest.PID); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				slog.Warn("terminate recovered execution process group", "path", path, "pid", manifest.PID, "error", err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("read recovered execution process identity", "path", path, "pid", manifest.PID, "error", err)
		} else if err == nil {
			slog.Warn("skip recovered execution process with changed identity", "path", path, "pid", manifest.PID)
		}
		recovered = append(recovered, RecoveredExecution{JobID: manifest.JobID, LeaseGeneration: manifest.LeaseGeneration, ExecutionID: manifest.ExecutionID})
		removeManifest(path)
	}
	return recovered, nil
}

func removeManifest(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove stale execution manifest", "path", path, "error", err)
	}
}

func terminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

func processStartTime(pid int) (string, error) {
	contents, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(contents[strings.LastIndex(string(contents), ")")+1:]))
	if len(fields) < 20 {
		return "", errors.New("process start time is unavailable")
	}
	return fields[19], nil
}
