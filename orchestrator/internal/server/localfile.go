package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalFileTaskSource is the seam's proof-of-concept second adapter: a
// TaskSource that never talks to GitHub (or any network) at all. ref (see
// TaskSource.ListTasks) is a directory path; every "*.json" file inside it is
// one task, decoded into localFileTask below and reduced to a neutral Task
// exactly the way the GitHub adapter reduces a decoded gh issue.
//
// It exists purely to demonstrate that the TaskSource seam is real rather
// than secretly GitHub-shaped -- it is not meant to be a production issue
// tracker. There is no eligibility label convention to speak of: unlike
// GitHub's agent:ready/agent:blocked labels, a local-file task simply states
// its own eligibility and priority directly, which is exactly the kind of
// provider-specific translation TaskSource is meant to let an adapter do on
// its own terms.
type LocalFileTaskSource struct{}

// localFileTask is one task file's on-disk shape. A file with no explicit
// external_id is identified by its own filename (without the .json suffix),
// which is what lets the simplest possible task source be "one file per
// task" with no other bookkeeping.
type localFileTask struct {
	ExternalID string `json:"external_id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	URL        string `json:"url"`
	Priority   int    `json:"priority"`
	Eligible   bool   `json:"eligible"`
	// State defaults to "open" when absent, so the minimal task file --
	// title only -- is schedulable without every field being spelled out.
	State string `json:"state"`
}

// ListTasks implements TaskSource by reading every "*.json" file directly
// inside the directory named by ref, in name order. A missing directory is an
// error, matching how the GitHub adapter fails loudly on an invalid
// repository reference rather than silently returning no tasks.
func (LocalFileTaskSource) ListTasks(_ context.Context, _ string, ref string) ([]Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("local file task source requires a directory path")
	}
	entries, err := os.ReadDir(ref)
	if err != nil {
		return nil, fmt.Errorf("read local task directory: %w", err)
	}
	var tasks []Task
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(ref, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read task file %s: %w", entry.Name(), err)
		}
		var file localFileTask
		if err := json.Unmarshal(contents, &file); err != nil {
			return nil, fmt.Errorf("decode task file %s: %w", entry.Name(), err)
		}
		if strings.TrimSpace(file.Title) == "" {
			return nil, fmt.Errorf("task file %s has no title", entry.Name())
		}
		externalID := strings.TrimSpace(file.ExternalID)
		if externalID == "" {
			externalID = strings.TrimSuffix(entry.Name(), ".json")
		}
		state := strings.ToLower(strings.TrimSpace(file.State))
		if state == "" {
			state = "open"
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat task file %s: %w", entry.Name(), err)
		}
		tasks = append(tasks, Task{
			ExternalID: externalID,
			Title:      file.Title,
			Body:       file.Body,
			URL:        file.URL,
			Priority:   file.Priority,
			// A closed task is never eligible, matching the GitHub adapter's
			// own rule that a closed issue cannot be "ready for work"
			// regardless of what its own fields claim.
			Eligible:  file.Eligible && state == "open",
			State:     state,
			CreatedAt: info.ModTime(),
			UpdatedAt: info.ModTime(),
			Raw:       json.RawMessage(contents),
		})
	}
	return tasks, nil
}
