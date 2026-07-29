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

// Push publishes a completed execution's work to the job's execution branch.
//
// The push is deliberately not forced, and that is a decision rather than an
// omission ([#156](https://github.com/alexandre-leites/moirai/issues/156)).
// Every execution of a workflow shares one branch name, so the second delivery
// to it — a repair iteration, a retry — is only accepted if it descends from
// what the first one published. It does: Manager.Prepare starts each workspace
// from the execution branch's tip rather than from the base revision (#136), so
// an ordinary delivery is a fast-forward.
//
// Forcing would make the same deliveries "succeed" without that guarantee, and
// would be wrong. The branch is not owned by the workflow: a human may amend
// the agent's work on it, and a runner leasing a concurrent execution of the
// same job publishes to it too. A force replaces such a commit with no trace,
// which is exactly what this repository has ruled out elsewhere. The only
// deliveries a plain push now rejects are those whose branch moved after the
// workspace was prepared — precisely the ones that must not be forced. Failing
// loudly is the right outcome there: the runner has not seen that work and
// cannot merge it unattended, and the execution that follows resumes from the
// published tip and delivers. `--force-with-lease` is no safer here, because
// the lease would be taken against a cache that Prepare has just fetched.
//
// PushWorkInProgress does force, and may: its ref is named after a single
// execution, which nothing else writes.
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
	if err := manager.gitWithEnv(ctx, extraEnvironment, pushCommand(workspace, "--set-upstream", "origin", branch)...); err != nil {
		return PushResult{}, fmt.Errorf("push branch: %w", err)
	}
	return PushResult{Branch: branch, Pushed: true}, nil
}

// pushCommand builds the argument list for a push from a workspace, with the
// remote's mirror setting neutralised for that one invocation.
//
// A managed_clone workspace is a worktree of the project cache, which
// Manager.prepareSource creates with "git clone --mirror". That sets
// remote.origin.mirror=true in the cache's config, which the worktree shares,
// and with it set Git treats every push to origin as a mirror push and refuses
// any push that names a refspec:
//
//	fatal: --mirror can't be combined with refspecs
//
// Every push the runner makes names one, so in managed_clone mode — the default
// repository mode — delivery, work-in-progress publication and remote branch
// cleanup all failed, and no runner could deliver anything. "git push
// --no-mirror" does not help: the configured value still wins. Only a config
// override does.
//
// The override is deliberately per invocation rather than a write to the
// cache's config: remote.origin.mirror keeps its value, so the cache still
// fetches as a mirror, nothing has to be migrated for caches already on disk,
// and existing_path workspaces — ordinary checkouts that never carry the
// setting — are unaffected either way. (The push does still write to the shared
// config by other means: --set-upstream records branch.<name>.remote and
// branch.<name>.merge there, as "git worktree add -B" already does.)
//
// Disabling it is also what makes these pushes mean what they say. A mirror
// push publishes every local ref, which would put the runner's private
// work-in-progress anchors (refs/moirai-wip/*, see RecordWorkInProgress) on the
// code host and delete any remote ref the cache happens not to have.
func pushCommand(workspace Workspace, arguments ...string) []string {
	command := []string{"-C", workspace.Repository, "-c", "remote.origin.mirror=false", "push"}
	return append(command, arguments...)
}

// RecordWorkInProgress points a local reference at the workspace's current HEAD.
//
// The commit of a failed run lives on the execution branch, which the next
// preparation of that job now continues from where it can (#136). The anchor
// covers the case it cannot: when the branch has been published elsewhere and
// this runner's copy does not contain that published tip, the preparation
// re-creates the branch there, and only a reference outside refs/heads still
// reaches the failed run's commit. Being outside refs/heads is also what keeps
// it from being checked out by a preparation or mistaken for a branch by a push.
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
	if err := manager.gitWithEnv(ctx, extraEnvironment, pushCommand(workspace, "--force", "origin", "HEAD:refs/heads/"+branch)...); err != nil {
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
	if err := manager.git(ctx, pushCommand(workspace, "origin", "--delete", branch)...); err != nil {
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
