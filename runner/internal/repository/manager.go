package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

type RepositoryMode string

const (
	RepositoryModeManagedClone RepositoryMode = "managed_clone"
	RepositoryModeExistingPath RepositoryMode = "existing_path"
)

type PrepareRequest struct {
	ProjectID           string
	JobID               string
	RepositoryMode      RepositoryMode
	RepositoryURL       string
	LocalRepositoryPath string
	DefaultBranch       string
	Branch              string
	// Environment carries the task packet's resolved environment references.
	// Any code-host credential it holds authenticates the clone and fetch that
	// populate the workspace; it is never written to disk or to a Git config.
	Environment map[string]string
}

type Workspace struct {
	Root       string
	Repository string
	Loop       string
}

type RevisionSummary struct {
	Revision     string
	ChangedFiles []string
}

type Manager struct {
	DataDirectory     string
	GitBinary         string
	CleanupAttempts   int
	CleanupRetryDelay time.Duration
	LockPollInterval  time.Duration
	GitCommitterName  string
	GitCommitterEmail string
	Sleep             func(time.Duration)
}

func (manager Manager) Prepare(ctx context.Context, request PrepareRequest) (Workspace, error) {
	if err := validateRequest(request); err != nil {
		return Workspace{}, err
	}
	root, err := manager.dataDirectory()
	if err != nil {
		return Workspace{}, err
	}
	unlock, err := manager.lockRepository(ctx, root, repositoryLockKey(request))
	if err != nil {
		return Workspace{}, err
	}
	defer unlock()
	workspace := Workspace{
		Root:       filepath.Join(root, "workspaces", "job-"+request.JobID),
		Repository: filepath.Join(root, "workspaces", "job-"+request.JobID, "repository"),
		Loop:       filepath.Join(root, "workspaces", "job-"+request.JobID, "repository", ".loop"),
	}
	if err := ensureContained(root, workspace.Root, workspace.Repository, workspace.Loop); err != nil {
		return Workspace{}, err
	}
	if _, err := os.Stat(workspace.Root); err == nil {
		if err := os.RemoveAll(workspace.Root); err != nil {
			return Workspace{}, fmt.Errorf("remove stale workspace for job %q: %w", request.JobID, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Workspace{}, fmt.Errorf("stat workspace: %w", err)
	}

	source, err := manager.prepareSource(ctx, root, request)
	if err != nil {
		return Workspace{}, err
	}
	if err := os.MkdirAll(workspace.Root, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	baseRevision := "origin/" + request.DefaultBranch
	if request.RepositoryMode == "" || request.RepositoryMode == RepositoryModeManagedClone {
		baseRevision = request.DefaultBranch
	}
	_ = manager.git(ctx, "-C", source, "worktree", "prune")
	if err := manager.git(ctx, "-C", source, "worktree", "add", "-B", request.Branch, workspace.Repository, baseRevision); err != nil {
		_ = os.RemoveAll(workspace.Root)
		return Workspace{}, fmt.Errorf("create worktree: %w", err)
	}
	if err := manager.excludeLoopArtifacts(ctx, workspace.Repository); err != nil {
		_ = os.RemoveAll(workspace.Root)
		return Workspace{}, err
	}
	if err := os.MkdirAll(workspace.Loop, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("create workspace task directory: %w", err)
	}
	return workspace, nil
}

// excludeLoopArtifacts writes ".loop/" to the repository's shared git exclude
// file so runner-authored artifacts (task packet, prompt, agent logs) never
// appear in "git status" or get swept up by a later "git add -A".
func (manager Manager) excludeLoopArtifacts(ctx context.Context, repository string) error {
	output, err := manager.gitOutput(ctx, "-C", repository, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git common directory: %w", err)
	}
	commonDirectory := strings.TrimSpace(statusLine(output))
	if commonDirectory == "" {
		return errors.New("git common directory is empty")
	}
	if !filepath.IsAbs(commonDirectory) {
		commonDirectory = filepath.Join(repository, commonDirectory)
	}
	excludePath := filepath.Join(commonDirectory, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o700); err != nil {
		return fmt.Errorf("create git info directory: %w", err)
	}
	contents, err := os.ReadFile(excludePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read git exclude file: %w", err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.TrimSpace(line) == "/.loop/" {
			return nil
		}
	}
	updated := string(contents)
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += "/.loop/\n"
	if err := os.WriteFile(excludePath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write git exclude file: %w", err)
	}
	return nil
}

func (manager Manager) prepareSource(ctx context.Context, root string, request PrepareRequest) (string, error) {
	credentials, err := credentialEnvironment(request.Environment)
	if err != nil {
		return "", err
	}
	if request.RepositoryMode == RepositoryModeExistingPath {
		source, err := filepath.EvalSymlinks(request.LocalRepositoryPath)
		if err != nil {
			return "", fmt.Errorf("resolve local repository path: %w", err)
		}
		info, err := os.Stat(source)
		if err != nil {
			return "", fmt.Errorf("stat local repository path: %w", err)
		}
		if !info.IsDir() {
			return "", errors.New("local repository path must be a directory")
		}
		if err := manager.gitWithEnv(ctx, credentials, "-C", source, "fetch", "--prune", "origin", request.DefaultBranch); err != nil {
			return "", fmt.Errorf("fetch local repository default branch: %w", err)
		}
		return source, nil
	}

	cache := filepath.Join(root, "repositories", "project-"+request.ProjectID, "repo.git")
	if err := ensureContained(root, cache); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return "", fmt.Errorf("create repository cache parent: %w", err)
	}
	if _, err := os.Stat(cache); errors.Is(err, os.ErrNotExist) {
		if err := manager.gitWithEnv(ctx, credentials, "clone", "--mirror", "--", request.RepositoryURL, cache); err != nil {
			return "", fmt.Errorf("clone repository cache: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("stat repository cache: %w", err)
	} else if err := manager.git(ctx, "-C", cache, "fsck", "--no-dangling"); err != nil {
		if err := os.RemoveAll(cache); err != nil {
			return "", fmt.Errorf("discard corrupt repository cache: %w", err)
		}
		if err := manager.gitWithEnv(ctx, credentials, "clone", "--mirror", "--", request.RepositoryURL, cache); err != nil {
			return "", fmt.Errorf("reclone corrupt repository cache: %w", err)
		}
	}
	if err := manager.gitWithEnv(ctx, credentials, "-C", cache, "fetch", "--prune", "origin", request.DefaultBranch); err != nil {
		return "", fmt.Errorf("fetch default branch: %w", err)
	}
	return cache, nil
}

// ReleaseBranch detaches a workspace's HEAD, leaving its commits and files in
// place. A retained workspace keeps its worktree registered with the source
// repository, and Git refuses to re-create a branch another worktree has
// checked out ("fatal: '<branch>' is already used by worktree at ..."). Since
// the orchestrator reuses one branch name for every execution of a workflow,
// retaining a failed workspace without detaching it would make the next attempt
// of that same workflow fail to prepare.
func (manager Manager) ReleaseBranch(ctx context.Context, workspace Workspace) error {
	root, err := manager.dataDirectory()
	if err != nil {
		return err
	}
	if err := ensureContained(root, workspace.Root, workspace.Repository); err != nil {
		return err
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "checkout", "--detach"); err != nil {
		return fmt.Errorf("detach retained workspace branch: %w", err)
	}
	return nil
}

func (manager Manager) Cleanup(ctx context.Context, projectID, jobID string) error {
	if !safeSegment(projectID) {
		return errors.New("project ID must contain only letters, digits, dots, underscores, or hyphens")
	}
	root, err := manager.dataDirectory()
	if err != nil {
		return err
	}
	unlock, err := manager.lockRepository(ctx, root, "managed:"+projectID)
	if err != nil {
		return err
	}
	defer unlock()
	cache := filepath.Join(root, "repositories", "project-"+projectID, "repo.git")
	if err := ensureContained(root, cache); err != nil {
		return err
	}
	return manager.cleanupWorkspace(ctx, cache, jobID)
}

func (manager Manager) CleanupExisting(ctx context.Context, localRepositoryPath, jobID string) error {
	if err := validateLocalRepositoryPath(localRepositoryPath); err != nil {
		return err
	}
	root, err := manager.dataDirectory()
	if err != nil {
		return err
	}
	unlock, err := manager.lockRepository(ctx, root, "existing:"+localRepositoryPath)
	if err != nil {
		return err
	}
	defer unlock()
	source, err := filepath.EvalSymlinks(localRepositoryPath)
	if err != nil {
		return fmt.Errorf("resolve local repository path: %w", err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("stat local repository path: %w", err)
	}
	if !info.IsDir() {
		return errors.New("local repository path must be a directory")
	}
	return manager.cleanupWorkspace(ctx, source, jobID)
}

func (manager Manager) cleanupWorkspace(ctx context.Context, source, jobID string) error {
	if !safeSegment(jobID) {
		return errors.New("job ID must contain only letters, digits, dots, underscores, or hyphens")
	}
	root, err := manager.dataDirectory()
	if err != nil {
		return err
	}
	workspaceRoot := filepath.Join(root, "workspaces", "job-"+jobID)
	repository := filepath.Join(workspaceRoot, "repository")
	if err := ensureContained(root, workspaceRoot, repository); err != nil {
		return err
	}
	attempts := manager.cleanupAttempts()
	var cleanupErr error
	for attempt := 0; attempt < attempts; attempt++ {
		cleanupErr = manager.cleanupWorkspaceOnce(ctx, source, workspaceRoot, repository)
		if cleanupErr == nil {
			return nil
		}
		if ctx.Err() != nil || attempt+1 == attempts {
			break
		}
		manager.sleep(manager.cleanupRetryDelay())
	}
	if quarantineErr := manager.quarantine(root, jobID, cleanupErr); quarantineErr != nil {
		return fmt.Errorf("cleanup workspace after %d attempts: %w; quarantine workspace: %v", attempts, cleanupErr, quarantineErr)
	}
	return fmt.Errorf("cleanup workspace after %d attempts: %w", attempts, cleanupErr)
}

func (manager Manager) cleanupWorkspaceOnce(ctx context.Context, source, workspaceRoot, workspaceRepository string) error {
	if _, err := os.Stat(workspaceRoot); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat workspace: %w", err)
	}
	if _, err := os.Stat(workspaceRepository); err == nil {
		if err := manager.git(ctx, "-C", source, "worktree", "remove", "--force", workspaceRepository); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat workspace repository: %w", err)
	}
	if err := os.RemoveAll(workspaceRoot); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}
	return nil
}

func (manager Manager) lockPollInterval() time.Duration {
	if manager.LockPollInterval > 0 {
		return manager.LockPollInterval
	}
	return 25 * time.Millisecond
}

func (manager Manager) cleanupAttempts() int {
	if manager.CleanupAttempts > 0 {
		return manager.CleanupAttempts
	}
	return 3
}

func (manager Manager) cleanupRetryDelay() time.Duration {
	if manager.CleanupRetryDelay > 0 {
		return manager.CleanupRetryDelay
	}
	return 250 * time.Millisecond
}

func (manager Manager) sleep(delay time.Duration) {
	if manager.Sleep != nil {
		manager.Sleep(delay)
		return
	}
	time.Sleep(delay)
}

func (manager Manager) quarantine(root, jobID string, cleanupErr error) error {
	directory := filepath.Join(root, "quarantine")
	path := filepath.Join(directory, "job-"+jobID+".json")
	if err := ensureContained(root, directory, path); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create quarantine directory: %w", err)
	}
	contents, err := json.Marshal(struct {
		JobID    string    `json:"jobId"`
		FailedAt time.Time `json:"failedAt"`
		Error    string    `json:"error"`
	}{JobID: jobID, FailedAt: time.Now().UTC(), Error: cleanupErr.Error()})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".job-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func (manager Manager) Snapshot(ctx context.Context, workspace Workspace) (RevisionSummary, error) {
	root, err := manager.dataDirectory()
	if err != nil {
		return RevisionSummary{}, err
	}
	if err := ensureContained(root, workspace.Root, workspace.Repository, workspace.Loop); err != nil {
		return RevisionSummary{}, err
	}
	revision, err := manager.gitOutput(ctx, "-C", workspace.Repository, "rev-parse", "HEAD")
	if err != nil {
		return RevisionSummary{}, fmt.Errorf("capture repository revision: %w", err)
	}
	status, err := manager.gitOutput(ctx, "-C", workspace.Repository, "status", "--porcelain=v1", "-z")
	if err != nil {
		return RevisionSummary{}, fmt.Errorf("capture repository changes: %w", err)
	}
	return RevisionSummary{Revision: strings.TrimSpace(statusLine(revision)), ChangedFiles: changedFiles(status)}, nil
}

func statusLine(output string) string {
	return strings.SplitN(output, "\n", 2)[0]
}

func changedFiles(status string) []string {
	seen := make(map[string]struct{})
	var files []string
	for _, entry := range strings.Split(status, "\x00") {
		if len(entry) < 4 {
			continue
		}
		path := strings.TrimSpace(entry[3:])
		if path == "" || isLoopArtifactPath(path) {
			continue
		}
		if _, exists := seen[path]; !exists {
			seen[path] = struct{}{}
			files = append(files, path)
		}
	}
	return files
}

func isLoopArtifactPath(path string) bool {
	return path == ".loop" || strings.HasPrefix(path, ".loop/")
}

func (manager Manager) git(ctx context.Context, arguments ...string) error {
	_, err := manager.gitOutput(ctx, arguments...)
	return err
}

func (manager Manager) gitOutput(ctx context.Context, arguments ...string) (string, error) {
	return manager.gitOutputWithEnv(ctx, nil, arguments...)
}

func (manager Manager) gitWithEnv(ctx context.Context, extraEnvironment []string, arguments ...string) error {
	_, err := manager.gitOutputWithEnv(ctx, extraEnvironment, arguments...)
	return err
}

func (manager Manager) gitOutputWithEnv(ctx context.Context, extraEnvironment []string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, manager.gitBinary(), arguments...)
	if len(extraEnvironment) > 0 {
		command.Env = append(os.Environ(), extraEnvironment...)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", arguments[0], err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (manager Manager) gitBinary() string {
	if manager.GitBinary == "" {
		return "git"
	}
	return manager.GitBinary
}

func repositoryLockKey(request PrepareRequest) string {
	if request.RepositoryMode == RepositoryModeExistingPath {
		return "existing:" + request.LocalRepositoryPath
	}
	return "managed:" + request.ProjectID
}

func (manager Manager) lockRepository(ctx context.Context, root, key string) (func(), error) {
	digest := sha256.Sum256([]byte(key))
	directory := filepath.Join(root, "locks")
	path := filepath.Join(directory, "repository-"+hex.EncodeToString(digest[:])+".lock")
	if err := ensureContained(root, directory, path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create repository lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open repository lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock repository: %w", err)
		}
		timer := time.NewTimer(manager.lockPollInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (manager Manager) dataDirectory() (string, error) {
	if manager.DataDirectory == "" {
		return "", errors.New("data directory is required")
	}
	root, err := filepath.Abs(filepath.Clean(manager.DataDirectory))
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	return root, nil
}

func validateRequest(request PrepareRequest) error {
	if !safeSegment(request.ProjectID) || !safeSegment(request.JobID) {
		return errors.New("project ID and job ID must contain only letters, digits, dots, underscores, or hyphens")
	}
	if !safeRef(request.DefaultBranch) || !safeRef(request.Branch) {
		return errors.New("branch names are invalid")
	}
	switch request.RepositoryMode {
	case "", RepositoryModeManagedClone:
		if request.RepositoryURL == "" || strings.HasPrefix(request.RepositoryURL, "-") || strings.ContainsAny(request.RepositoryURL, "\x00\r\n") {
			return errors.New("repository URL is invalid")
		}
		if request.LocalRepositoryPath != "" {
			return errors.New("local repository path is only valid for existing_path mode")
		}
	case RepositoryModeExistingPath:
		if request.RepositoryURL != "" {
			return errors.New("repository URL is only valid for managed_clone mode")
		}
		if err := validateLocalRepositoryPath(request.LocalRepositoryPath); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported repository mode %q", request.RepositoryMode)
	}
	return nil
}

func validateLocalRepositoryPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("local repository path must be an absolute path without control characters")
	}
	return nil
}

func safeSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !(unicode.IsLetter(character) || unicode.IsDigit(character) || character == '.' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func safeRef(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.ContainsAny(value, " ~^:?*[\\\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if !safeSegment(segment) {
			return false
		}
	}
	return true
}

func ensureContained(root string, paths ...string) error {
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return fmt.Errorf("path %q escapes data directory", path)
		}
	}
	return nil
}
