package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var credentialEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

type CommitResult struct {
	Committed bool
	Revision  string
}

type PushResult struct {
	Branch string
	Pushed bool
}

type BranchCleanupResult struct {
	Branch  string
	Deleted bool
}

func (manager Manager) Commit(ctx context.Context, workspace Workspace, message string) (CommitResult, error) {
	if err := validateWorkspace(workspace); err != nil {
		return CommitResult{}, err
	}
	if !safeCommitMessage(message) {
		return CommitResult{}, errors.New("commit message is invalid")
	}
	status, err := manager.gitOutput(ctx, "-C", workspace.Repository, "status", "--porcelain=v1", "-z")
	if err != nil {
		return CommitResult{}, fmt.Errorf("inspect commit changes: %w", err)
	}
	if status == "" {
		revision, err := manager.gitOutput(ctx, "-C", workspace.Repository, "rev-parse", "HEAD")
		if err != nil {
			return CommitResult{}, fmt.Errorf("read current revision: %w", err)
		}
		return CommitResult{Revision: strings.TrimSpace(revision)}, nil
	}
	// Runner artifacts are kept out of the commit by the worktree's git exclude
	// file (see Manager.excludeLoopArtifacts), and unstaged again afterwards in
	// case that exclude is ever missing. They are deliberately not named as
	// negative pathspecs: a pathspec that explicitly matches an ignored path
	// makes "git add" report it and exit non-zero, which failed every commit in
	// a prepared workspace — exactly where .loop always exists.
	if err := manager.git(ctx, "-C", workspace.Repository, "add", "-A", "--", "."); err != nil {
		return CommitResult{}, fmt.Errorf("stage repository changes: %w", err)
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "reset", "--quiet", "--", ".loop"); err != nil {
		return CommitResult{}, fmt.Errorf("unstage runner artifacts: %w", err)
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "-c", "user.name="+manager.committerName(), "-c", "user.email="+manager.committerEmail(), "commit", "-m", message); err != nil {
		return CommitResult{}, fmt.Errorf("commit repository changes: %w", err)
	}
	revision, err := manager.gitOutput(ctx, "-C", workspace.Repository, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, fmt.Errorf("read committed revision: %w", err)
	}
	return CommitResult{Committed: true, Revision: strings.TrimSpace(revision)}, nil
}

func (manager Manager) Push(ctx context.Context, workspace Workspace, branch string, environment map[string]string) (PushResult, error) {
	if err := validateWorkspace(workspace); err != nil {
		return PushResult{}, err
	}
	if !safeRef(branch) {
		return PushResult{}, errors.New("push branch is invalid")
	}
	extraEnvironment, err := credentialEnvironment(environment)
	if err != nil {
		return PushResult{}, err
	}
	if err := manager.gitWithEnv(ctx, extraEnvironment, "-C", workspace.Repository, "push", "--set-upstream", "origin", branch); err != nil {
		return PushResult{}, fmt.Errorf("push branch: %w", err)
	}
	return PushResult{Branch: branch, Pushed: true}, nil
}

// RecordWorkInProgress points a local reference at the workspace's current HEAD.
// The commit of a failed run lives on the execution branch, and the next
// preparation of that job re-creates the branch from the base revision, so
// without an anchor the work becomes unreachable in the runner's own repository.
// The reference is written outside refs/heads so no preparation can check it out
// and no push mistakes it for a branch.
func (manager Manager) RecordWorkInProgress(ctx context.Context, workspace Workspace, reference string) error {
	if err := validateWorkspace(workspace); err != nil {
		return err
	}
	if !safeReference(reference) {
		return errors.New("work-in-progress reference is invalid")
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "update-ref", reference, "HEAD"); err != nil {
		return fmt.Errorf("record work-in-progress reference: %w", err)
	}
	return nil
}

// PushWorkInProgress publishes the workspace's current HEAD to a dedicated
// remote branch so the work a failed run produced survives the workspace. It
// deliberately differs from Push: no upstream is set and the local branch is not
// named, so the deliverable agent branch is never advanced by a failed run.
//
// The remote ref is owned by one execution, so the push is forced: an execution
// redelivered after a crash must be able to replace its own earlier remains
// rather than fail on a non-fast-forward.
func (manager Manager) PushWorkInProgress(ctx context.Context, workspace Workspace, branch string, environment map[string]string) (PushResult, error) {
	if err := validateWorkspace(workspace); err != nil {
		return PushResult{}, err
	}
	if !safeRef(branch) {
		return PushResult{}, errors.New("work-in-progress push branch is invalid")
	}
	extraEnvironment, err := credentialEnvironment(environment)
	if err != nil {
		return PushResult{}, err
	}
	if err := manager.gitWithEnv(ctx, extraEnvironment, "-C", workspace.Repository, "push", "--force", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return PushResult{}, fmt.Errorf("push work-in-progress branch: %w", err)
	}
	return PushResult{Branch: branch, Pushed: true}, nil
}

// credentialEnvironment renders the resolved task environment (which may carry
// a code-host credential such as GITHUB_TOKEN) as extra environment variables
// for a Git subprocess. When a GitHub token is present it also configures the
// GitHub HTTPS authorization header through GIT_CONFIG_* so clone, fetch, and
// push authenticate without the token ever appearing in an argument list.
func credentialEnvironment(environment map[string]string) ([]string, error) {
	extra := make([]string, 0, len(environment))
	for name, value := range environment {
		if !credentialEnvironmentName.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("git credential environment is invalid")
		}
		extra = append(extra, name+"="+value)
	}
	if token := environment["GITHUB_TOKEN"]; token != "" {
		credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		extra = append(extra,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+credential,
		)
	}
	return extra, nil
}

func (manager Manager) CleanupRemoteBranch(ctx context.Context, workspace Workspace, branch string) (BranchCleanupResult, error) {
	if err := validateWorkspace(workspace); err != nil {
		return BranchCleanupResult{}, err
	}
	if !safeRef(branch) {
		return BranchCleanupResult{}, errors.New("cleanup branch is invalid")
	}
	remote, err := manager.gitOutput(ctx, "-C", workspace.Repository, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return BranchCleanupResult{}, fmt.Errorf("inspect remote branch: %w", err)
	}
	result := BranchCleanupResult{Branch: branch}
	if strings.TrimSpace(remote) == "" {
		return result, nil
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "push", "origin", "--delete", branch); err != nil {
		return BranchCleanupResult{}, fmt.Errorf("delete remote branch: %w", err)
	}
	result.Deleted = true
	return result, nil
}

func (manager Manager) committerName() string {
	if manager.GitCommitterName != "" {
		return manager.GitCommitterName
	}
	return "moirai-runner"
}

func (manager Manager) committerEmail() string {
	if manager.GitCommitterEmail != "" {
		return manager.GitCommitterEmail
	}
	return "moirai-runner@localhost"
}

func validateWorkspace(workspace Workspace) error {
	if workspace.Repository == "" || !filepath.IsAbs(workspace.Repository) {
		return errors.New("repository workspace is invalid")
	}
	info, err := os.Stat(workspace.Repository)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("repository workspace is unavailable: %w", err)
	}
	return nil
}

// safeReference validates a fully qualified reference name: a refs/ prefix plus
// path segments that satisfy the same rules as a branch name.
func safeReference(reference string) bool {
	return strings.HasPrefix(reference, "refs/") && safeRef(reference)
}

func safeCommitMessage(message string) bool {
	return message != "" && len(message) <= 4096 && strings.TrimSpace(message) == message && !strings.ContainsAny(message, "\x00\r")
}
