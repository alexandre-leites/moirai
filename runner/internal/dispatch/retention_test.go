package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/repository"
)

type sweepWorkspaceManager struct {
	mu         sync.Mutex
	workspace  repository.Workspace
	cleaned    []string
	cleanupErr error
}

func (manager *sweepWorkspaceManager) Prepare(context.Context, repository.PrepareRequest) (repository.Workspace, error) {
	for _, path := range []string{manager.workspace.Root, manager.workspace.Repository, manager.workspace.Loop} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return repository.Workspace{}, err
		}
	}
	return manager.workspace, nil
}

func (manager *sweepWorkspaceManager) Cleanup(_ context.Context, _, jobID string) error {
	return manager.record(jobID)
}

func (manager *sweepWorkspaceManager) CleanupExisting(_ context.Context, _, jobID string) error {
	return manager.record(jobID)
}

func (manager *sweepWorkspaceManager) ReleaseBranch(context.Context, repository.Workspace) error {
	return nil
}

func (manager *sweepWorkspaceManager) record(jobID string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.cleanupErr != nil {
		return manager.cleanupErr
	}
	manager.cleaned = append(manager.cleaned, jobID)
	return nil
}

func (manager *sweepWorkspaceManager) cleanedJobs() []string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]string(nil), manager.cleaned...)
}

// retainWorkspace writes a registry record and its workspace directory, as a
// finished execution would have left them.
func retainWorkspace(t *testing.T, directory, jobID string, age time.Duration) string {
	t.Helper()
	root := filepath.Join(directory, "workspaces", "job-"+jobID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	contents, err := json.Marshal(retainedWorkspace{
		JobID:      jobID,
		ProjectID:  "project-1",
		Mode:       "managed_clone",
		Root:       root,
		Status:     "failed",
		RetainedAt: time.Now().UTC().Add(-age),
	})
	if err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(directory, "retained")
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, retentionRecordName(jobID)), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func retainedJobs(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(directory, "retained", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := make([]string, 0, len(entries))
	for _, entry := range entries {
		jobs = append(jobs, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(entry), "job-"), ".json"))
	}
	sort.Strings(jobs)
	return jobs
}

func TestSweepReleasesRetainedWorkspacesPastTheAgeBound(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{}
	retainWorkspace(t, directory, "old", 48*time.Hour)
	retainWorkspace(t, directory, "recent", time.Minute)
	dispatcher := Dispatcher{Workspaces: manager, Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 24 * time.Hour, MaxWorkspaces: 10}}

	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	if got := manager.cleanedJobs(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("cleaned jobs = %#v, want only the aged-out workspace", got)
	}
	if got := retainedJobs(t, directory); len(got) != 1 || got[0] != "recent" {
		t.Fatalf("remaining retention records = %#v", got)
	}
}

func TestSweepReleasesTheOldestRetainedWorkspacesPastTheCountBound(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{}
	for index, jobID := range []string{"oldest", "middle", "newest"} {
		retainWorkspace(t, directory, jobID, time.Duration(3-index)*time.Hour)
	}
	dispatcher := Dispatcher{Workspaces: manager, Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 72 * time.Hour, MaxWorkspaces: 1}}

	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	got := manager.cleanedJobs()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "middle" || got[1] != "oldest" {
		t.Fatalf("cleaned jobs = %#v, want the two oldest", got)
	}
	if remaining := retainedJobs(t, directory); len(remaining) != 1 || remaining[0] != "newest" {
		t.Fatalf("remaining retention records = %#v", remaining)
	}
}

func TestSweepReleasesRetainedWorkspacesUnderDiskPressure(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{}
	for index, jobID := range []string{"oldest", "middle", "newest"} {
		retainWorkspace(t, directory, jobID, time.Duration(3-index)*time.Hour)
	}
	var probes int
	dispatcher := Dispatcher{
		Workspaces:       manager,
		MinimumFreeBytes: 100,
		DiskPath:         directory,
		AvailableBytes: func(string) (uint64, error) {
			probes++
			// Free space recovers only after two workspaces are released.
			if probes > 2 {
				return 200, nil
			}
			return 10, nil
		},
		Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 72 * time.Hour, MaxWorkspaces: 10},
	}

	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	got := manager.cleanedJobs()
	if len(got) != 2 || got[0] != "oldest" || got[1] != "middle" {
		t.Fatalf("cleaned jobs = %#v, want the oldest released first", got)
	}
	if remaining := retainedJobs(t, directory); len(remaining) != 1 || remaining[0] != "newest" {
		t.Fatalf("remaining retention records = %#v", remaining)
	}
}

func TestSweepKeepsRecordsItCouldNotRelease(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{cleanupErr: errors.New("worktree is busy")}
	retainWorkspace(t, directory, "stuck", 48*time.Hour)
	dispatcher := Dispatcher{Workspaces: manager, Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: time.Hour, MaxWorkspaces: 10}}

	err := dispatcher.SweepRetainedWorkspaces(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stuck") {
		t.Fatalf("SweepRetainedWorkspaces() error = %v, want it to name the job", err)
	}
	if got := retainedJobs(t, directory); len(got) != 1 || got[0] != "stuck" {
		t.Fatalf("retention records = %#v, want the unreleased workspace kept for a later sweep", got)
	}
}

func TestSweepForgetsRecordsWhoseWorkspaceIsGone(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{}
	root := retainWorkspace(t, directory, "removed", time.Minute)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	dispatcher := Dispatcher{Workspaces: manager, Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 72 * time.Hour, MaxWorkspaces: 10}}

	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	if got := retainedJobs(t, directory); len(got) != 0 {
		t.Fatalf("retention records = %#v, want the stale record dropped", got)
	}
	if got := manager.cleanedJobs(); len(got) != 0 {
		t.Fatalf("cleaned jobs = %#v, want no cleanup for a workspace that is already gone", got)
	}
}

func TestSweepDiscardsUnusableRetentionRecords(t *testing.T) {
	directory := t.TempDir()
	records := filepath.Join(directory, "retained")
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"job-broken.json":     "{not json",
		"job-mismatched.json": `{"jobId":"other","root":"/tmp"}`,
	} {
		if err := os.WriteFile(filepath.Join(records, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &sweepWorkspaceManager{}
	dispatcher := Dispatcher{Workspaces: manager, Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: time.Hour, MaxWorkspaces: 10}}

	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	if got := retainedJobs(t, directory); len(got) != 0 {
		t.Fatalf("retention records = %#v, want the unusable records discarded", got)
	}
	if got := manager.cleanedJobs(); len(got) != 0 {
		t.Fatalf("cleaned jobs = %#v, want no cleanup driven by an unusable record", got)
	}
}

// TestExecuteSweepsRetainedWorkspacesBeforeMeasuringFreeSpace proves the bound
// is enforced on the path that creates workspaces, so retained forensics cost
// the next execution capacity rather than blocking it.
func TestExecuteSweepsRetainedWorkspacesBeforeMeasuringFreeSpace(t *testing.T) {
	directory := t.TempDir()
	retainWorkspace(t, directory, "old", 96*time.Hour)
	workspace := repository.Workspace{
		Root:       filepath.Join(directory, "workspaces", "job-1"),
		Repository: filepath.Join(directory, "workspaces", "job-1", "repository"),
		Loop:       filepath.Join(directory, "workspaces", "job-1", "repository", ".loop"),
	}
	manager := &sweepWorkspaceManager{workspace: workspace}
	dispatcher := Dispatcher{
		Workspaces:       manager,
		Backend:          &backend{result: agents.Result{Status: "completed"}},
		MinimumFreeBytes: 100,
		DiskPath:         directory,
		// Releasing the aged-out workspace is what frees the space this
		// execution needs, so the run only proceeds if the sweep ran first.
		AvailableBytes: func(string) (uint64, error) {
			if len(manager.cleanedJobs()) > 0 {
				return 200, nil
			}
			return 10, nil
		},
		Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 24 * time.Hour, MaxWorkspaces: 10},
	}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := manager.cleanedJobs(); len(got) == 0 || got[0] != "old" {
		t.Fatalf("cleaned jobs = %#v, want the aged-out workspace released before the disk check", got)
	}
}

// TestExecuteForgetsAStaleRecordForTheJobItIsAboutToRun keeps a reused job ID
// from pointing a later sweep at a live workspace.
func TestExecuteForgetsAStaleRecordForTheJobItIsAboutToRun(t *testing.T) {
	directory := t.TempDir()
	retainWorkspace(t, directory, "job-1", time.Minute)
	workspace := repository.Workspace{
		Root:       filepath.Join(directory, "workspaces", "job-job-1"),
		Repository: filepath.Join(directory, "workspaces", "job-job-1", "repository"),
		Loop:       filepath.Join(directory, "workspaces", "job-job-1", "repository", ".loop"),
	}
	manager := &sweepWorkspaceManager{workspace: workspace}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    &backend{result: agents.Result{Status: "completed"}},
		Retention:  RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: 72 * time.Hour, MaxWorkspaces: 10},
	}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// The completed run is not retained, so no record may survive for its job.
	if got := retainedJobs(t, directory); len(got) != 0 {
		t.Fatalf("retention records = %#v, want the stale record dropped by preparation", got)
	}
	if got := manager.cleanedJobs(); len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("cleaned jobs = %#v, want only the finished execution's own cleanup", got)
	}
}

// TestSweepSkipsWorkspacesClaimedByARunningExecution is the guard against the
// hazard that makes a registry alone insufficient: one job ID serves every
// execution of a workflow run, so a record written by an earlier execution
// names exactly the path the next one prepares. Without the claim, a sweep
// running for another job (LOOP_RUNNER_CAPACITY > 1) would delete a live
// worktree out from under a running agent.
func TestSweepSkipsWorkspacesClaimedByARunningExecution(t *testing.T) {
	directory := t.TempDir()
	manager := &sweepWorkspaceManager{}
	retainWorkspace(t, directory, "running", 96*time.Hour)
	retainWorkspace(t, directory, "finished", 96*time.Hour)
	active := NewActiveWorkspaces()
	dispatcher := Dispatcher{
		Workspaces: manager,
		Active:     active,
		Retention:  RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: time.Hour, MaxWorkspaces: 10},
	}

	release := active.Claim("running")
	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	if got := manager.cleanedJobs(); len(got) != 1 || got[0] != "finished" {
		t.Fatalf("cleaned jobs = %#v, want the claimed workspace left alone", got)
	}
	if got := retainedJobs(t, directory); len(got) != 1 || got[0] != "running" {
		t.Fatalf("retention records = %#v, want the claimed job's record kept", got)
	}

	// Once the execution releases its claim the record is swept normally.
	release()
	if err := dispatcher.SweepRetainedWorkspaces(context.Background()); err != nil {
		t.Fatalf("SweepRetainedWorkspaces() error = %v", err)
	}
	if got := manager.cleanedJobs(); len(got) != 2 {
		t.Fatalf("cleaned jobs = %#v, want the released workspace swept", got)
	}
}

// TestExecuteClaimsItsWorkspaceBeforeSweeping proves the claim covers the whole
// execution, so a concurrent sweep cannot act on this job at any point.
func TestExecuteClaimsItsWorkspaceBeforeSweeping(t *testing.T) {
	directory := t.TempDir()
	workspace := repository.Workspace{
		Root:       filepath.Join(directory, "workspaces", "job-job-1"),
		Repository: filepath.Join(directory, "workspaces", "job-job-1", "repository"),
		Loop:       filepath.Join(directory, "workspaces", "job-job-1", "repository", ".loop"),
	}
	manager := &sweepWorkspaceManager{workspace: workspace}
	active := NewActiveWorkspaces()
	claimedDuringExecution := false
	dispatcher := Dispatcher{
		Workspaces: manager,
		Active:     active,
		Backend: &callbackBackend{onExecute: func() {
			claimedDuringExecution = active.claimed("job-1")
		}},
		Retention: RetentionPolicy{KeepFailed: true, Directory: directory, MaxAge: time.Hour, MaxWorkspaces: 10},
	}

	if _, err := dispatcher.Execute(context.Background(), validLease()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !claimedDuringExecution {
		t.Fatal("the execution did not claim its workspace against the retention sweep")
	}
	if active.claimed("job-1") {
		t.Fatal("the claim outlived the execution")
	}
}

func TestActiveWorkspacesNestsClaimsAndTolerantOfNil(t *testing.T) {
	var absent *ActiveWorkspaces
	absent.Claim("job-1")()
	if absent.claimed("job-1") {
		t.Fatal("a nil tracker reported a claim")
	}
	active := NewActiveWorkspaces()
	first, second := active.Claim("job-1"), active.Claim("job-1")
	first()
	if !active.claimed("job-1") {
		t.Fatal("releasing one of two claims dropped the job")
	}
	second()
	if active.claimed("job-1") {
		t.Fatal("releasing every claim left the job claimed")
	}
}
