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

var pushEnvironmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

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
	if err := manager.git(ctx, "-C", workspace.Repository, "add", "-A", "--", ".", ":!.loop", ":!.loop/**"); err != nil {
		return CommitResult{}, fmt.Errorf("stage repository changes: %w", err)
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
	extraEnvironment, err := pushEnvironment(environment)
	if err != nil {
		return PushResult{}, err
	}
	if err := manager.gitWithEnv(ctx, extraEnvironment, "-C", workspace.Repository, "push", "--set-upstream", "origin", branch); err != nil {
		return PushResult{}, fmt.Errorf("push branch: %w", err)
	}
	return PushResult{Branch: branch, Pushed: true}, nil
}

// pushEnvironment renders resolved task environment (which may carry a push
// credential such as GITHUB_TOKEN) as extra environment variables for the
// "git push" subprocess, so a configured credential helper or askpass hook
// on the runner host can authenticate the push.
func pushEnvironment(environment map[string]string) ([]string, error) {
	extra := make([]string, 0, len(environment))
	for name, value := range environment {
		if !pushEnvironmentName.MatchString(name) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("push credential environment is invalid")
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

func safeCommitMessage(message string) bool {
	return message != "" && len(message) <= 4096 && strings.TrimSpace(message) == message && !strings.ContainsAny(message, "\x00\r")
}
