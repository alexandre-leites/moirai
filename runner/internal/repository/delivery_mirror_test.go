package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The delivery tests in delivery_test.go build their workspace with a plain
// "git init", which is an ordinary checkout and therefore cannot observe the
// property that broke every push in the default repository mode: a
// managed_clone workspace is a worktree of a "git clone --mirror" cache, whose
// config carries remote.origin.mirror=true, and Git refuses any push that names
// a refspec while that is set ("fatal: --mirror can't be combined with
// refspecs"). Issue #147.
//
// Every test here therefore prepares a real managed_clone workspace through
// Manager.Prepare and reads the result back out of the origin repository, which
// is the only place that can prove a push actually delivered something.

// TestPushFromManagedCloneWorkspacePublishesTheDeliveryBranch is the first
// acceptance criterion of #147: a completed developer execution in
// managed_clone mode publishes its branch and reports pushed: true.
func TestPushFromManagedCloneWorkspacePublishesTheDeliveryBranch(t *testing.T) {
	const branch = "agent/issue-147/run-1"
	root := t.TempDir()
	origin := managedCloneOrigin(t, root)
	manager, workspace := prepareManagedCloneWorkspace(t, root, origin, branch)

	// An earlier failed execution left its private anchor in the shared cache,
	// as RecordWorkInProgress does for every failed run. It is local to the
	// runner and must never be published: a push that reverted to mirror
	// semantics would carry it to the code host along with everything else.
	if err := manager.RecordWorkInProgress(context.Background(), workspace, "refs/moirai-wip/execution-earlier"); err != nil {
		t.Fatalf("RecordWorkInProgress() error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(workspace.Repository, "delivered.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "loop: implement issue 147")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}

	// The resolved environment carries a code-host credential (#109), so the
	// push runs on exactly the path a real delivery takes.
	push, err := manager.Push(context.Background(), workspace, branch, map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if !push.Pushed || push.Branch != branch {
		t.Fatalf("Push() = %#v, want branch %q pushed", push, branch)
	}
	if published := readRemoteRevision(t, origin, "refs/heads/"+branch); published != commit.Revision {
		t.Fatalf("origin refs/heads/%s = %q, want the committed revision %q", branch, published, commit.Revision)
	}
	if contents := readRemoteFile(t, origin, branch, "delivered.txt"); contents != "delivered\n" {
		t.Fatalf("delivered file in origin = %q", contents)
	}
	assertRemoteReferences(t, origin, "refs/heads/main", "refs/heads/"+branch)
}

// TestPushWorkInProgressFromManagedCloneWorkspacePublishesTheWipBranch is the
// second acceptance criterion of #147: a failed execution in managed_clone mode
// publishes wip/<executionId>. It follows the same order as
// Dispatcher.retainWorkInProgress — commit, anchor locally, then publish — so
// the local anchor is present when the push happens, which is what lets the
// final reference assertion prove the push carried the named refspec alone
// rather than becoming a mirror push of everything the cache holds.
func TestPushWorkInProgressFromManagedCloneWorkspacePublishesTheWipBranch(t *testing.T) {
	// The two names the dispatcher derives from an execution ID; see
	// workInProgressBranch and workInProgressReference in internal/dispatch.
	const (
		deliveryBranch          = "agent/issue-147/run-1"
		workInProgressBranch    = "wip/execution-147"
		workInProgressReference = "refs/moirai-wip/execution-147"
	)
	root := t.TempDir()
	origin := managedCloneOrigin(t, root)
	manager, workspace := prepareManagedCloneWorkspace(t, root, origin, deliveryBranch)

	if err := os.WriteFile(filepath.Join(workspace.Repository, "partial.go"), []byte("package partial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (147)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	if err := manager.RecordWorkInProgress(context.Background(), workspace, workInProgressReference); err != nil {
		t.Fatalf("RecordWorkInProgress() error = %v", err)
	}

	pushed, err := manager.PushWorkInProgress(context.Background(), workspace, workInProgressBranch, map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil {
		t.Fatalf("PushWorkInProgress() error = %v", err)
	}
	if !pushed.Pushed || pushed.Branch != workInProgressBranch {
		t.Fatalf("PushWorkInProgress() = %#v, want branch %q pushed", pushed, workInProgressBranch)
	}
	if published := readRemoteRevision(t, origin, "refs/heads/"+workInProgressBranch); published != commit.Revision {
		t.Fatalf("origin refs/heads/%s = %q, want the retained commit %q", workInProgressBranch, published, commit.Revision)
	}

	// A failed run must not advance the delivery branch, and the runner's
	// private anchor must not reach the code host. Both would happen if the
	// mirror setting were left in force and the refspec dropped to appease it.
	assertRemoteReferences(t, origin, "refs/heads/main", "refs/heads/"+workInProgressBranch)

	// An execution redelivered after a crash replaces its own earlier remains
	// (#100): --force plus a refspec is the exact combination Git rejected.
	//
	// The retry is deliberately built on the base revision rather than on the
	// commit already published, so the second push is a genuine
	// non-fast-forward — which is the only shape that actually needs --force.
	// A retry that simply added another commit would succeed without it, and
	// would prove nothing about the flag.
	runRealGit(t, workspace.Repository, "reset", "--quiet", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(workspace.Repository, "different.go"), []byte("package different\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (147)")
	if err != nil || !second.Committed {
		t.Fatalf("Commit() = %#v, %v", second, err)
	}
	if second.Revision == commit.Revision {
		t.Fatal("the retry produced the same revision; this no longer tests a non-fast-forward push")
	}
	if isAncestor(t, workspace.Repository, commit.Revision, second.Revision) {
		t.Fatal("the retry commit descends from the published one; this no longer tests a non-fast-forward push")
	}
	if _, err := manager.PushWorkInProgress(context.Background(), workspace, workInProgressBranch, nil); err != nil {
		t.Fatalf("PushWorkInProgress() retry error = %v", err)
	}
	if published := readRemoteRevision(t, origin, "refs/heads/"+workInProgressBranch); published != second.Revision {
		t.Fatalf("origin refs/heads/%s = %q, want the redelivered commit %q", workInProgressBranch, published, second.Revision)
	}
	assertRemoteReferences(t, origin, "refs/heads/main", "refs/heads/"+workInProgressBranch)
}

// TestCleanupRemoteBranchFromManagedCloneWorkspaceDeletesTheBranch covers the
// third refspec-bearing push. It is not named in the acceptance criteria, but
// it failed for the same reason, and a delete that cannot run leaves an
// abandoned branch on the code host for every execution.
func TestCleanupRemoteBranchFromManagedCloneWorkspaceDeletesTheBranch(t *testing.T) {
	const branch = "agent/issue-147/run-1"
	root := t.TempDir()
	origin := managedCloneOrigin(t, root)
	manager, workspace := prepareManagedCloneWorkspace(t, root, origin, branch)

	if err := os.WriteFile(filepath.Join(workspace.Repository, "delivered.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if commit, err := manager.Commit(context.Background(), workspace, "loop: implement issue 147"); err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	if _, err := manager.Push(context.Background(), workspace, branch, nil); err != nil {
		t.Fatalf("Push() error = %v", err)
	}

	cleanup, err := manager.CleanupRemoteBranch(context.Background(), workspace, branch)
	if err != nil {
		t.Fatalf("CleanupRemoteBranch() error = %v", err)
	}
	if !cleanup.Deleted || cleanup.Branch != branch {
		t.Fatalf("CleanupRemoteBranch() = %#v, want branch %q deleted", cleanup, branch)
	}
	if remaining := readRemoteRevision(t, origin, "refs/heads/"+branch); remaining != "" {
		t.Fatalf("origin still holds refs/heads/%s at %q", branch, remaining)
	}
	assertRemoteReferences(t, origin, "refs/heads/main")
}

// TestManagedCloneMirrorConfigurationDoesNotBreakFetchOrLsRemote records the
// other half of the investigation #147 asked for: the mirror setting is a push
// setting, so the remaining refspec-bearing commands the runner issues against
// origin — the fetch that refreshes the cache and the ls-remote that guards the
// branch deletion — keep working from inside a mirror. It is here to stop a
// future change from "fixing" them too, or from assuming they were ever broken.
func TestManagedCloneMirrorConfigurationDoesNotBreakFetchOrLsRemote(t *testing.T) {
	root := t.TempDir()
	origin := managedCloneOrigin(t, root)
	manager, workspace := prepareManagedCloneWorkspace(t, root, origin, "agent/issue-147/run-1")

	cache := filepath.Join(root, "data", "repositories", "project-project-1", "repo.git")
	if err := manager.git(context.Background(), "-C", cache, "fetch", "--prune", "origin", "main"); err != nil {
		t.Fatalf("fetch from a mirror cache failed: %v", err)
	}
	if err := manager.git(context.Background(), "-C", workspace.Repository, "fetch", "--prune", "origin", "main"); err != nil {
		t.Fatalf("fetch from a mirror worktree failed: %v", err)
	}
	if _, err := manager.gitOutput(context.Background(), "-C", workspace.Repository, "ls-remote", "--heads", "origin", "refs/heads/main"); err != nil {
		t.Fatalf("ls-remote from a mirror worktree failed: %v", err)
	}
}

// managedCloneOrigin creates a bare repository holding a single commit on main,
// standing in for the code host a managed_clone project is cloned from.
func managedCloneOrigin(t *testing.T, root string) string {
	t.Helper()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	runRealGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	runRealGit(t, root, "init", "-q", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, seed, "add", "README.md")
	runRealGit(t, seed, "-c", "user.name=Initial", "-c", "user.email=initial@example.invalid", "commit", "-qm", "initial")
	runRealGit(t, seed, "remote", "add", "origin", origin)
	runRealGit(t, seed, "push", "-q", "origin", "main")
	return origin
}

// prepareManagedCloneWorkspace prepares a workspace the way the dispatcher does
// for the default repository mode, and asserts the property that makes this
// file worth having: the workspace really is a worktree of a mirror clone, so a
// push from it really does meet remote.origin.mirror=true.
func prepareManagedCloneWorkspace(t *testing.T, root, origin, branch string) (Manager, Workspace) {
	t.Helper()
	manager := Manager{
		DataDirectory:     filepath.Join(root, "data"),
		GitCommitterName:  "Moirai Runner",
		GitCommitterEmail: "runner@example.invalid",
	}
	workspace, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:      "project-1",
		JobID:          "job-1",
		RepositoryMode: RepositoryModeManagedClone,
		RepositoryURL:  origin,
		DefaultBranch:  "main",
		Branch:         branch,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	mirror := readGitConfiguration(t, workspace.Repository, "remote.origin.mirror")
	if mirror != "true" {
		t.Fatalf("prepared workspace has remote.origin.mirror = %q, want %q: the managed_clone cache is no longer a mirror clone, so these tests no longer reproduce the push failure of issue #147 and must be re-targeted at whatever the cache is now", mirror, "true")
	}
	return manager, workspace
}

// readGitConfiguration returns a configuration value as the workspace resolves
// it, or "" when it is unset — "git config --get" exits non-zero for a missing
// key, which is not an error here.
func readGitConfiguration(t *testing.T, directory, key string) string {
	t.Helper()
	command := exec.Command("git", "-C", directory, "config", "--get", key)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// isAncestor reports whether one revision is reachable from another, which is
// what decides whether a push is a fast-forward.
func isAncestor(t *testing.T, repository, ancestor, descendant string) bool {
	t.Helper()
	command := exec.Command("git", "-C", repository, "merge-base", "--is-ancestor", ancestor, descendant)
	return command.Run() == nil
}

// readRemoteFile reads a file from a reference in a bare repository.
func readRemoteFile(t *testing.T, remote, reference, path string) string {
	t.Helper()
	command := exec.Command("git", "-C", remote, "show", reference+":"+path)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git show %s:%s in %s: %v", reference, path, remote, err)
	}
	return string(output)
}

// assertRemoteReferences requires the remote to hold exactly the given
// references. Exactness is the point: a push that silently reverted to mirror
// semantics would succeed while publishing every ref the cache holds, and an
// assertion that only looked for the expected branch would not notice.
func assertRemoteReferences(t *testing.T, remote string, want ...string) {
	t.Helper()
	command := exec.Command("git", "-C", remote, "for-each-ref", "--format=%(refname)")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git for-each-ref in %s: %v", remote, err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			got = append(got, line)
		}
	}
	expected := slices.Clone(want)
	slices.Sort(got)
	slices.Sort(expected)
	if !slices.Equal(got, expected) {
		t.Fatalf("origin references = %v, want exactly %v", got, expected)
	}
}
