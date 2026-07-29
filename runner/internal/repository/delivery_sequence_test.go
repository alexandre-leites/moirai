package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #156: only the first completed execution of a workflow ever reached the
// code host. Every execution of a workflow shares one branch name — the
// orchestrator persists a single `branch_name` per workflow run and reuses it
// for repair iterations and retries — so once `Prepare` had rewound that branch
// onto the base revision, the next delivery's commit was not a descendant of
// what the previous one published and `git push` refused it:
//
//	! [rejected] agent/x -> agent/x (non-fast-forward)
//
// The fix is that `Prepare` starts each workspace from the execution branch's
// tip rather than from the base revision (#136), which makes every subsequent
// delivery an ordinary fast-forward. That is a property of the *pair* Prepare
// and Push, and neither half's tests observe it: the preparation tests stop at
// the workspace revision and never push, and the delivery tests push exactly
// once. These tests close that gap by running the whole cycle repeatedly and
// reading the result back out of the origin repository, which is the only place
// that can prove a delivery actually landed.
//
// The rejected alternative is pinned here as well, because `--force` on the
// delivery push is the fix a future non-fast-forward report will tempt someone
// into: the execution branch looks like it belongs to the workflow. It does
// not. Anyone may push to it — a human amending the agent's work, a second
// runner that was re-offered the job after its lease expired — and a force
// replaces such a commit with no trace, which is exactly what this repository
// has already ruled out for repudiated commits. Because the workspace now
// starts from the published tip, forcing buys nothing: the only deliveries a
// plain push rejects are the ones whose branch moved after Prepare resolved it,
// and those are precisely the ones that must not be forced. See Push's own
// comment for why `--force-with-lease` is not the middle ground it looks like.

// deliveryJob drives one workflow's executions against a single execution
// branch, the way the dispatcher does: prepare, modify, commit, optionally
// push, clean up.
type deliveryJob struct {
	t       *testing.T
	manager Manager
	request PrepareRequest
	origin  string
}

// newDeliveryJob builds a job whose repository mode matches the argument, so
// every sequence below runs against both modes: managed_clone workspaces are
// worktrees of a `git clone --mirror` cache and existing_path workspaces are
// worktrees of an ordinary checkout, and the two resolve the execution branch
// through different references.
func newDeliveryJob(t *testing.T, mode RepositoryMode, branch string) deliveryJob {
	t.Helper()
	root := t.TempDir()
	origin := managedCloneOrigin(t, root)
	request := PrepareRequest{
		ProjectID:      "project-1",
		JobID:          "job-1",
		RepositoryMode: mode,
		RepositoryURL:  origin,
		DefaultBranch:  "main",
		Branch:         branch,
	}
	if mode == RepositoryModeExistingPath {
		checkout := filepath.Join(root, "checkout")
		runRealGit(t, root, "clone", "-q", origin, checkout)
		request.RepositoryURL = ""
		request.LocalRepositoryPath = checkout
	}
	return deliveryJob{t: t, manager: newRealGitManager(root), request: request, origin: origin}
}

// start prepares a workspace for the next execution of the job and writes one
// file into it, standing in for whatever the agent produced.
func (job deliveryJob) start(name, contents string) Workspace {
	job.t.Helper()
	workspace, err := job.manager.Prepare(context.Background(), job.request)
	if err != nil {
		job.t.Fatalf("Prepare() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Repository, name), []byte(contents), 0o600); err != nil {
		job.t.Fatal(err)
	}
	return workspace
}

// commit records the execution's work on the execution branch.
func (job deliveryJob) commit(workspace Workspace, message string) string {
	job.t.Helper()
	result, err := job.manager.Commit(context.Background(), workspace, message)
	if err != nil || !result.Committed {
		job.t.Fatalf("Commit() = %#v, %v", result, err)
	}
	return result.Revision
}

// finish releases the workspace, which the runner does at the end of every
// execution. The branch, not the directory, is what the next execution
// inherits.
func (job deliveryJob) finish() {
	job.t.Helper()
	if job.request.RepositoryMode == RepositoryModeExistingPath {
		if err := job.manager.CleanupExisting(context.Background(), job.request.LocalRepositoryPath, job.request.JobID); err != nil {
			job.t.Fatalf("CleanupExisting() error = %v", err)
		}
		return
	}
	if err := job.manager.Cleanup(context.Background(), job.request.ProjectID, job.request.JobID); err != nil {
		job.t.Fatalf("Cleanup() error = %v", err)
	}
}

// deliver runs a complete developer execution: prepare, produce a file, commit,
// push, release the workspace. It returns the delivered revision and requires
// the origin to hold it, so a push that reported success without publishing
// anything cannot pass.
func (job deliveryJob) deliver(name, contents, message string) string {
	job.t.Helper()
	workspace := job.start(name, contents)
	revision := job.commit(workspace, message)
	result, err := job.manager.Push(context.Background(), workspace, job.request.Branch, nil)
	if err != nil {
		job.t.Fatalf("Push() error = %v", err)
	}
	if !result.Pushed || result.Branch != job.request.Branch {
		job.t.Fatalf("Push() = %#v", result)
	}
	if got := realGitRevision(job.t, job.origin, "refs/heads/"+job.request.Branch); got != revision {
		job.t.Fatalf("published branch tip = %q, want the delivered revision %q", got, revision)
	}
	job.finish()
	return revision
}

// publishOutsideTheRunner puts a commit on the execution branch through a
// clone the runner knows nothing about — a human amending the agent's work, or
// another runner leasing a concurrent execution — and returns the tip it left.
func (job deliveryJob) publishOutsideTheRunner(name, contents, message string) string {
	job.t.Helper()
	outside := filepath.Join(job.t.TempDir(), "outside")
	runRealGit(job.t, filepath.Dir(outside), "clone", "-q", "-b", job.request.Branch, job.origin, outside)
	if err := os.WriteFile(filepath.Join(outside, name), []byte(contents), 0o600); err != nil {
		job.t.Fatal(err)
	}
	runRealGit(job.t, outside, "add", name)
	runRealGit(job.t, outside, "-c", "user.name=Outside", "-c", "user.email=outside@example.invalid", "commit", "-qm", message)
	runRealGit(job.t, outside, "push", "-q", "origin", job.request.Branch)
	return realGitRevision(job.t, job.origin, "refs/heads/"+job.request.Branch)
}

// TestConsecutiveDeliveriesToOneExecutionBranchAllReachTheCodeHost is the first
// two acceptance criteria of #156, in both repository modes: consecutive
// completed executions of one workflow all publish, and the branch on the code
// host ends up holding the latest execution's work.
//
// Three deliveries rather than the two the criterion names: preparation
// re-resolves the branch from scratch every time, so only a third run shows
// that the state a delivery leaves behind is one the next preparation can
// continue from, rather than one that happened to work once.
//
// The failed execution between them is the repair loop the issue is about. It
// commits to the execution branch and publishes only to its own
// `wip/<executionId>` branch, so the delivery that follows must inherit its
// work while the delivery branch stays exactly where the last delivery left it.
// A preparation that resolved the start point from the remote alone would push
// a perfectly good fast-forward here and still silently drop that commit, which
// is why the files are asserted and not just the ancestry.
func TestConsecutiveDeliveriesToOneExecutionBranchAllReachTheCodeHost(t *testing.T) {
	for _, mode := range []RepositoryMode{RepositoryModeManagedClone, RepositoryModeExistingPath} {
		t.Run(string(mode), func(t *testing.T) {
			const branch = "agent/issue-156/run-1"
			job := newDeliveryJob(t, mode, branch)

			first := job.deliver("first.txt", "first\n", "developer: first attempt (156)")

			// A repair iteration fails. It commits to the execution branch and
			// publishes to a branch of its own; the delivery branch stays where
			// the first delivery left it.
			failed := job.start("repair.txt", "repair\n")
			failedRevision := job.commit(failed, "wip(failed): developer: repair (156)")
			if err := job.manager.RecordWorkInProgress(context.Background(), failed, "refs/moirai-wip/execution-2"); err != nil {
				t.Fatalf("RecordWorkInProgress() error = %v", err)
			}
			if _, err := job.manager.PushWorkInProgress(context.Background(), failed, "wip/execution-2", nil); err != nil {
				t.Fatalf("PushWorkInProgress() error = %v", err)
			}
			if got := realGitRevision(t, job.origin, "refs/heads/"+branch); got != first {
				t.Fatalf("a failed run moved the delivery branch to %q, want %q", got, first)
			}
			job.finish()

			second := job.deliver("second.txt", "second\n", "developer: second attempt (156)")
			third := job.deliver("third.txt", "third\n", "developer: third attempt (156)")

			// The branch on the code host holds the latest execution's work,
			// read back out of the origin repository rather than from the
			// workspace that produced it.
			if got := realGitRevision(t, job.origin, "refs/heads/"+branch); got != third {
				t.Fatalf("final published tip = %q, want the third delivery %q", got, third)
			}
			for _, step := range []struct{ file, contents string }{
				{"first.txt", "first\n"},
				{"repair.txt", "repair\n"},
				{"second.txt", "second\n"},
				{"third.txt", "third\n"},
			} {
				if got := readRemoteFile(t, job.origin, "refs/heads/"+branch, step.file); got != step.contents {
					t.Fatalf("published %s = %q, want %q", step.file, got, step.contents)
				}
			}
			// Every delivery built on the one before it, which is what makes the
			// pushes fast-forwards rather than replacements.
			if !isAncestor(t, job.origin, first, second) || !isAncestor(t, job.origin, second, third) {
				t.Fatalf("deliveries are not a chain: %s -> %s -> %s", first, second, third)
			}
			if !isAncestor(t, job.origin, failedRevision, second) {
				t.Fatalf("the failed run's work at %s was not inherited by the delivery that followed it", failedRevision)
			}
			// The failed run's own branch is left where it was: the delivery
			// branch advancing must not rewrite it.
			if got := readRemoteRevision(t, job.origin, "refs/heads/wip/execution-2"); got != failedRevision {
				t.Fatalf("work-in-progress branch = %q, want %q", got, failedRevision)
			}
		})
	}
}

// TestDeliveryKeepsACommitTheRunnerDidNotMake is the fourth acceptance
// criterion of #156. Someone else — a human reviewing the branch, or another
// runner — pushes to the execution branch between two executions. The next
// delivery has to carry that commit forward, not replace it, which is the
// property a forced push would remove.
func TestDeliveryKeepsACommitTheRunnerDidNotMake(t *testing.T) {
	for _, mode := range []RepositoryMode{RepositoryModeManagedClone, RepositoryModeExistingPath} {
		t.Run(string(mode), func(t *testing.T) {
			const branch = "agent/issue-156/run-1"
			job := newDeliveryJob(t, mode, branch)
			first := job.deliver("first.txt", "first\n", "developer: first attempt (156)")

			amended := job.publishOutsideTheRunner("review.txt", "reviewed\n", "amend the agent's work by hand")

			second := job.deliver("second.txt", "second\n", "developer: second attempt (156)")
			if !isAncestor(t, job.origin, amended, second) {
				t.Fatalf("the second delivery discarded the commit %s that the runner did not make", amended)
			}
			if got := readRemoteFile(t, job.origin, "refs/heads/"+branch, "review.txt"); got != "reviewed\n" {
				t.Fatalf("published review.txt = %q, want it preserved", got)
			}
			if !isAncestor(t, job.origin, first, second) {
				t.Fatalf("the second delivery discarded the first at %s", first)
			}
		})
	}
}

// TestDeliveryIsRejectedRatherThanForcedWhenTheBranchMovedUnderIt covers the
// one interleaving that a fast-forward push still refuses: the execution branch
// moved after the workspace was prepared, so this execution's commit is not a
// descendant of what the branch now holds. Refusing is the correct outcome —
// the runner has not seen that work and cannot merge it unattended — and it is
// the difference between this fix and a forced push, which would report success
// while deleting the commit.
func TestDeliveryIsRejectedRatherThanForcedWhenTheBranchMovedUnderIt(t *testing.T) {
	for _, mode := range []RepositoryMode{RepositoryModeManagedClone, RepositoryModeExistingPath} {
		t.Run(string(mode), func(t *testing.T) {
			const branch = "agent/issue-156/run-1"
			job := newDeliveryJob(t, mode, branch)
			first := job.deliver("first.txt", "first\n", "developer: first attempt (156)")

			workspace := job.start("second.txt", "second\n")
			// Only now, with the workspace already prepared at the first
			// delivery, does the branch move on the code host.
			concurrent := job.publishOutsideTheRunner("concurrent.txt", "concurrent\n", "a concurrent publisher")

			job.commit(workspace, "developer: second attempt (156)")
			if _, err := job.manager.Push(context.Background(), workspace, branch, nil); err == nil {
				t.Fatal("Push() overwrote a branch that moved under the workspace instead of failing")
			} else if !strings.Contains(err.Error(), "push branch") {
				t.Fatalf("Push() error = %v, want it to name the failed push", err)
			}
			if got := realGitRevision(t, job.origin, "refs/heads/"+branch); got != concurrent {
				t.Fatalf("published tip = %q, want the concurrent publisher's commit %q left intact", got, concurrent)
			}
			if !isAncestor(t, job.origin, first, concurrent) {
				t.Fatalf("the concurrent commit %s does not build on the first delivery %s", concurrent, first)
			}
			job.finish()

			// The execution that follows resumes from the published tip, so the
			// retry delivers rather than failing again: a rejection costs one
			// execution, it does not strand the workflow.
			retried := job.deliver("retry.txt", "retry\n", "developer: retry (156)")
			if !isAncestor(t, job.origin, concurrent, retried) {
				t.Fatalf("the retry at %s did not build on the concurrent commit %s", retried, concurrent)
			}
			if got := readRemoteFile(t, job.origin, "refs/heads/"+branch, "concurrent.txt"); got != "concurrent\n" {
				t.Fatalf("published concurrent.txt = %q, want it preserved by the retry", got)
			}
		})
	}
}

// TestPushNamesNoForcingFlag pins the shape of the delivery push itself.
//
// It is deliberately redundant, and the redundancy was measured rather than
// assumed: with `Prepare` intact, adding `--force` fails
// TestDeliveryIsRejectedRatherThanForcedWhenTheBranchMovedUnderIt in both
// modes, and adding `--force-with-lease` fails three of the real-git subtests.
// Neither variant would slip past.
//
// What this adds is the message. A forced push surfaces there as a Git
// transcript in a delivery that should have failed, several assertions deep in
// a test about branch movement; here it surfaces at the flag itself, naming why
// the flag is wrong, in milliseconds and without Git. The temptation this
// guards against — reaching for `--force` the next time a non-fast-forward is
// reported — is answered at exactly the line where someone would type it.
func TestPushNamesNoForcingFlag(t *testing.T) {
	binary, recorded := fakeGit(t)
	workspace := Workspace{Repository: filepath.Join(t.TempDir(), "repository")}
	if err := os.MkdirAll(workspace.Repository, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := Manager{GitBinary: binary}
	if _, err := manager.Push(context.Background(), workspace, "agent/issue-156/run-1", nil); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	commands := readGitCommands(t, recorded)
	if len(commands) != 1 {
		t.Fatalf("recorded commands = %#v, want exactly the push", commands)
	}
	for _, argument := range commands[0] {
		if strings.HasPrefix(argument, "--force") || strings.HasPrefix(argument, "+") {
			t.Fatalf("delivery push names %q: the execution branch is not owned by the workflow, and forcing it deletes commits the runner did not make (#156)", argument)
		}
	}
}
