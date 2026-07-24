package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	if err := manager.git(ctx, "-C", workspace.Repository, "add", "-A"); err != nil {
		return CommitResult{}, fmt.Errorf("stage repository changes: %w", err)
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "commit", "-m", message); err != nil {
		return CommitResult{}, fmt.Errorf("commit repository changes: %w", err)
	}
	revision, err := manager.gitOutput(ctx, "-C", workspace.Repository, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, fmt.Errorf("read committed revision: %w", err)
	}
	return CommitResult{Committed: true, Revision: strings.TrimSpace(revision)}, nil
}

func (manager Manager) Push(ctx context.Context, workspace Workspace, branch string) (PushResult, error) {
	if err := validateWorkspace(workspace); err != nil {
		return PushResult{}, err
	}
	if !safeRef(branch) {
		return PushResult{}, errors.New("push branch is invalid")
	}
	if err := manager.git(ctx, "-C", workspace.Repository, "push", "--set-upstream", "origin", branch); err != nil {
		return PushResult{}, fmt.Errorf("push branch: %w", err)
	}
	return PushResult{Branch: branch, Pushed: true}, nil
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
