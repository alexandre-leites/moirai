package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loop-engineering/runner/internal/taskpacket"
)

// RetentionPolicy decides which finished workspaces survive cleanup and bounds
// the disk the surviving ones may hold. Retaining a failed workspace is what
// lets a retry inspect the previous attempt's worktree, terminal result, and
// agent logs, so the default keeps failed runs — but "keep everything" would
// eventually fill the runner's disk, so every retained workspace is registered
// and released again by an age, count, and free-space bounded sweep.
type RetentionPolicy struct {
	KeepSucceeded bool
	KeepFailed    bool
	KeepAbandoned bool
	// MaxAge releases a retained workspace once it is older than this. Zero
	// disables the age bound.
	MaxAge time.Duration
	// MaxWorkspaces bounds how many retained workspaces may coexist, releasing
	// the oldest first. Zero disables the count bound.
	MaxWorkspaces int
	// Directory is the runner data directory. The retention registry lives in
	// <Directory>/retained. Retention is bookkeeping-first: without a directory
	// a retained workspace could never be found again and therefore never be
	// bounded, so the dispatcher cleans it up instead of leaking it.
	Directory string
}

// retainedWorkspace is the registry record for one retained workspace. It
// carries everything the sweep needs to release the workspace through the same
// path the dispatcher uses, so a swept worktree is unregistered from its source
// repository rather than merely unlinked.
type retainedWorkspace struct {
	JobID      string    `json:"jobId"`
	ProjectID  string    `json:"projectId"`
	Mode       string    `json:"mode"`
	LocalPath  string    `json:"localPath"`
	Root       string    `json:"root"`
	Status     string    `json:"status"`
	RetainedAt time.Time `json:"retainedAt"`
}

func (dispatcher Dispatcher) retentionDirectory() string {
	if dispatcher.Retention.Directory == "" {
		return ""
	}
	return filepath.Join(dispatcher.Retention.Directory, "retained")
}

func retentionRecordName(jobID string) string {
	return "job-" + jobID + ".json"
}

// recordRetainedWorkspace registers a workspace the retention policy kept, so a
// later sweep can release it. A registry write failure is reported to the
// caller, which then cleans the workspace up rather than leaking an untracked
// directory.
func (dispatcher Dispatcher) recordRetainedWorkspace(packet taskpacket.Packet, root, status string) error {
	directory := dispatcher.retentionDirectory()
	if directory == "" {
		return errors.New("workspace retention requires a runner data directory")
	}
	if strings.ContainsAny(packet.JobID, `/\`) || packet.JobID == "" {
		return fmt.Errorf("job ID %q cannot be registered for retention", packet.JobID)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create workspace retention directory: %w", err)
	}
	contents, err := json.Marshal(retainedWorkspace{
		JobID:      packet.JobID,
		ProjectID:  packet.Repository.ProjectID,
		Mode:       packet.Repository.Mode,
		LocalPath:  packet.Repository.LocalPath,
		Root:       root,
		Status:     status,
		RetainedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("encode retained workspace record: %w", err)
	}
	return writePrivateFile(filepath.Join(directory, retentionRecordName(packet.JobID)), append(contents, '\n'))
}

// forgetRetainedWorkspace drops a job's registry record without touching the
// workspace itself. It runs before every preparation so that a job ID reused by
// a new execution can never be released by a concurrent sweep acting on the
// previous execution's record.
func (dispatcher Dispatcher) forgetRetainedWorkspace(jobID string) {
	directory := dispatcher.retentionDirectory()
	if directory == "" || jobID == "" || strings.ContainsAny(jobID, `/\`) {
		return
	}
	if err := os.Remove(filepath.Join(directory, retentionRecordName(jobID))); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("could not drop a retained workspace record", "job_id", jobID, "error", err)
	}
}

// SweepRetainedWorkspaces releases retained workspaces that exceed the policy's
// age or count bound, and keeps releasing the oldest while free disk is below
// the runner's minimum. It only ever considers registered records, which are
// written after an execution finishes, so a running execution's workspace is
// never a candidate.
func (dispatcher Dispatcher) SweepRetainedWorkspaces(ctx context.Context) error {
	if dispatcher.retentionDirectory() == "" || dispatcher.Workspaces == nil {
		return nil
	}
	records, err := dispatcher.loadRetainedWorkspaces()
	if err != nil {
		return err
	}
	sort.Slice(records, func(first, second int) bool { return records[first].RetainedAt.Before(records[second].RetainedAt) })
	var failures []error
	release := func(record retainedWorkspace) bool {
		if err := dispatcher.releaseRetainedWorkspace(ctx, record); err != nil {
			failures = append(failures, err)
			return false
		}
		slog.Info("released a retained workspace", "job_id", record.JobID, "status", record.Status, "retained_at", record.RetainedAt)
		return true
	}

	now := time.Now().UTC()
	kept := make([]retainedWorkspace, 0, len(records))
	for _, record := range records {
		if _, err := os.Stat(record.Root); errors.Is(err, os.ErrNotExist) {
			dispatcher.forgetRetainedWorkspace(record.JobID)
			continue
		}
		if dispatcher.Retention.MaxAge > 0 && now.Sub(record.RetainedAt) >= dispatcher.Retention.MaxAge && release(record) {
			continue
		}
		kept = append(kept, record)
	}
	if limit := dispatcher.Retention.MaxWorkspaces; limit > 0 && len(kept) > limit {
		remaining := make([]retainedWorkspace, 0, len(kept))
		for index, record := range kept {
			if index < len(kept)-limit && release(record) {
				continue
			}
			remaining = append(remaining, record)
		}
		kept = remaining
	}
	for dispatcher.MinimumFreeBytes > 0 && dispatcher.AvailableBytes != nil && len(kept) > 0 {
		available, err := dispatcher.AvailableBytes(dispatcher.DiskPath)
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect workspace disk space: %w", err))
			break
		}
		if available >= dispatcher.MinimumFreeBytes || !release(kept[0]) {
			break
		}
		kept = kept[1:]
	}
	return errors.Join(failures...)
}

func (dispatcher Dispatcher) loadRetainedWorkspaces() ([]retainedWorkspace, error) {
	directory := dispatcher.retentionDirectory()
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read retained workspace records: %w", err)
	}
	records := make([]retainedWorkspace, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("could not read a retained workspace record", "path", path, "error", err)
			continue
		}
		var record retainedWorkspace
		if err := json.Unmarshal(contents, &record); err != nil || record.JobID == "" || record.Root == "" || entry.Name() != retentionRecordName(record.JobID) {
			slog.Warn("discarding an unusable retained workspace record", "path", path, "error", err)
			if err := os.Remove(path); err != nil {
				slog.Warn("could not remove an unusable retained workspace record", "path", path, "error", err)
			}
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (dispatcher Dispatcher) releaseRetainedWorkspace(ctx context.Context, record retainedWorkspace) error {
	if err := dispatcher.cleanupWorkspace(ctx, record.Mode, record.ProjectID, record.LocalPath, record.JobID); err != nil {
		return fmt.Errorf("release retained workspace for job %q: %w", record.JobID, err)
	}
	dispatcher.forgetRetainedWorkspace(record.JobID)
	return nil
}

// retentionStatus classifies a finished execution for the retention policy.
func retentionStatus(ctx context.Context, result Result, executeErr error) string {
	if ctx.Err() != nil {
		return "abandoned"
	}
	if executeErr == nil && result.Status == "completed" {
		return "succeeded"
	}
	return "failed"
}
