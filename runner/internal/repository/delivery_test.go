package repository

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommitStagesChangesAndPushesValidatedBranch(t *testing.T) {
	binary, recorded := fakeGit(t)
	t.Setenv("LOOP_GIT_STATUS", "1")
	workspace := Workspace{Repository: filepath.Join(t.TempDir(), "repository")}
	if err := os.MkdirAll(workspace.Repository, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := Manager{GitBinary: binary}
	result, err := manager.Commit(context.Background(), workspace, "loop: implement issue 7")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed {
		t.Fatal("Commit() did not report committed changes")
	}
	push, err := manager.Push(context.Background(), workspace, "agent/issue-7/run-1", map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil || !push.Pushed {
		t.Fatalf("Push() = %#v, %v", push, err)
	}
	commands := readGitCommands(t, recorded)
	joined := make([]string, len(commands))
	for index, command := range commands {
		joined[index] = strings.Join(command, " ")
	}
	if !strings.Contains(strings.Join(joined, "\n"), "add -A -- .") || !strings.Contains(strings.Join(joined, "\n"), "reset --quiet -- .loop") || !strings.Contains(strings.Join(joined, "\n"), "commit -m loop: implement issue 7") || !strings.Contains(strings.Join(joined, "\n"), "push --set-upstream origin agent/issue-7/run-1") {
		t.Fatalf("delivery commands = %#v", commands)
	}
}

func TestPushAuthenticatesGitHubHTTPSRemoteFromResolvedEnvironment(t *testing.T) {
	binary, recorded := fakeGit(t)
	workspace := Workspace{Repository: filepath.Join(t.TempDir(), "repository")}
	if err := os.MkdirAll(workspace.Repository, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := Manager{GitBinary: binary}

	push, err := manager.Push(context.Background(), workspace, "agent/issue-7/run-1", map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil || !push.Pushed {
		t.Fatalf("Push() = %#v, %v", push, err)
	}

	commands := readGitCommands(t, recorded)
	environments := readGitEnvironments(t, recorded)
	if len(commands) != 1 || len(environments) != 1 {
		t.Fatalf("recorded commands = %#v", commands)
	}
	credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:token-value"))
	for _, entry := range []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic " + credential,
		"GITHUB_TOKEN=token-value",
	} {
		if !gitEnvironmentContains(environments[0], entry) {
			t.Fatalf("push environment is missing %q: %#v", entry, environments[0])
		}
	}
	if strings.Contains(strings.Join(commands[0], " "), "token-value") {
		t.Fatalf("push arguments leaked the credential: %#v", commands[0])
	}
}

// TestCredentialEnvironmentConfiguresRealGit proves the injected GIT_CONFIG_*
// variables are actually honoured by git, so an authenticated clone, fetch, or
// push really does send the Authorization header rather than merely carrying
// an unused environment variable.
func TestCredentialEnvironmentConfiguresRealGit(t *testing.T) {
	extra, err := credentialEnvironment(map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "config", "--get", "http.https://github.com/.extraheader")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), extra...)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git config --get: %v", err)
	}
	credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:token-value"))
	if strings.TrimSpace(string(output)) != "AUTHORIZATION: basic "+credential {
		t.Fatalf("git resolved extraheader = %q", output)
	}
}

func TestCommitAndPushUseRunnerIdentityWithoutAmbientGitConfiguration(t *testing.T) {
	root := t.TempDir()
	working := filepath.Join(root, "working")
	remote := filepath.Join(root, "remote.git")
	runRealGit(t, root, "init", "-b", "main", working)
	if err := os.WriteFile(filepath.Join(working, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, working, "add", "README.md")
	runRealGit(t, working, "-c", "user.name=Initial", "-c", "user.email=initial@example.invalid", "commit", "-m", "initial")
	runRealGit(t, root, "init", "--bare", remote)
	runRealGit(t, working, "remote", "add", "origin", remote)
	runRealGit(t, working, "push", "origin", "main")
	if err := os.WriteFile(filepath.Join(working, "delivery.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := Manager{GitCommitterName: "Moirai Runner", GitCommitterEmail: "runner@example.invalid"}
	workspace := Workspace{Repository: working}
	result, err := manager.Commit(context.Background(), workspace, "runner delivery")
	if err != nil || !result.Committed {
		t.Fatalf("Commit() = %#v, %v", result, err)
	}
	pushed, err := manager.Push(context.Background(), workspace, "main", map[string]string{"GITHUB_TOKEN": "token-value"})
	if err != nil || !pushed.Pushed {
		t.Fatalf("Push() = %#v, %v", pushed, err)
	}
	command := exec.Command("git", "--git-dir", remote, "log", "-1", "--format=%an <%ae>", "refs/heads/main")
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != "Moirai Runner <runner@example.invalid>" {
		t.Fatalf("pushed commit identity = %q, %v", output, err)
	}
}

func runRealGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func TestCleanupRemoteBranchDeletesOnlyExistingBranch(t *testing.T) {
	binary, recorded := fakeGit(t)
	t.Setenv("LOOP_GIT_REMOTE_BRANCH", "1")
	workspace := Workspace{Repository: t.TempDir()}
	manager := Manager{GitBinary: binary}
	result, err := manager.CleanupRemoteBranch(context.Background(), workspace, "agent/issue-7/run-1")
	if err != nil || !result.Deleted {
		t.Fatalf("CleanupRemoteBranch() = %#v, %v", result, err)
	}
	commands := readGitCommands(t, recorded)
	if len(commands) != 2 || !strings.Contains(strings.Join(commands[1], " "), "push origin --delete agent/issue-7/run-1") {
		t.Fatalf("cleanup commands = %#v", commands)
	}
	t.Setenv("LOOP_GIT_REMOTE_BRANCH", "")
	result, err = manager.CleanupRemoteBranch(context.Background(), workspace, "agent/issue-7/run-1")
	if err != nil || result.Deleted {
		t.Fatalf("idempotent CleanupRemoteBranch() = %#v, %v", result, err)
	}
}

func TestDeliveryAdaptersRejectUnsafeInputs(t *testing.T) {
	manager := Manager{}
	workspace := Workspace{Repository: t.TempDir()}
	if _, err := manager.Commit(context.Background(), workspace, "bad\nmessage"); err == nil {
		t.Fatal("Commit() accepted unsafe message")
	}
	if _, err := manager.Push(context.Background(), workspace, "../unsafe", nil); err == nil {
		t.Fatal("Push() accepted unsafe branch")
	}
	if _, err := manager.Push(context.Background(), workspace, "agent/issue-7/run-1", map[string]string{"bad name": "value"}); err == nil {
		t.Fatal("Push() accepted unsafe credential environment name")
	}
}

// TestPushWorkInProgressPublishesFailedWorkWithoutAdvancingTheDeliveryBranch
// runs against real Git because the guarantee is about refs: the work of a
// failed run has to reach the remote so a retry on another runner can build on
// it, while the branch that means "delivered" must stay where it was.
func TestPushWorkInProgressPublishesFailedWorkWithoutAdvancingTheDeliveryBranch(t *testing.T) {
	root := t.TempDir()
	working := filepath.Join(root, "working")
	remote := filepath.Join(root, "remote.git")
	runRealGit(t, root, "init", "-q", "-b", "main", working)
	if err := os.WriteFile(filepath.Join(working, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, working, "add", "README.md")
	runRealGit(t, working, "-c", "user.name=Initial", "-c", "user.email=initial@example.invalid", "commit", "-qm", "initial")
	runRealGit(t, root, "init", "-q", "--bare", remote)
	runRealGit(t, working, "remote", "add", "origin", remote)
	runRealGit(t, working, "push", "-q", "origin", "main")
	runRealGit(t, working, "checkout", "-q", "-b", "agent/issue-7/run-1")

	manager := Manager{GitCommitterName: "Moirai Runner", GitCommitterEmail: "runner@example.invalid"}
	workspace := Workspace{Repository: working}
	if err := os.WriteFile(filepath.Join(working, "partial.go"), []byte("package partial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (7)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}

	pushed, err := manager.PushWorkInProgress(context.Background(), workspace, "wip/execution-1", nil)
	if err != nil || !pushed.Pushed || pushed.Branch != "wip/execution-1" {
		t.Fatalf("PushWorkInProgress() = %#v, %v", pushed, err)
	}

	remoteWorkInProgress := readRemoteRevision(t, remote, "refs/heads/wip/execution-1")
	if remoteWorkInProgress != commit.Revision {
		t.Fatalf("remote work-in-progress ref = %q, want the retained commit %q", remoteWorkInProgress, commit.Revision)
	}
	if delivered := readRemoteRevision(t, remote, "refs/heads/agent/issue-7/run-1"); delivered != "" {
		t.Fatalf("a failed run advanced the delivery branch to %q", delivered)
	}
	upstream := exec.Command("git", "rev-parse", "--symbolic-full-name", "@{upstream}")
	upstream.Dir = working
	if output, err := upstream.CombinedOutput(); err == nil {
		t.Fatalf("work-in-progress push set an upstream: %s", output)
	}

	// A redelivered execution replaces its own remains rather than failing.
	if err := os.WriteFile(filepath.Join(working, "more.go"), []byte("package more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (7)")
	if err != nil || !second.Committed {
		t.Fatalf("Commit() = %#v, %v", second, err)
	}
	if _, err := manager.PushWorkInProgress(context.Background(), workspace, "wip/execution-1", nil); err != nil {
		t.Fatalf("PushWorkInProgress() retry error = %v", err)
	}
	if got := readRemoteRevision(t, remote, "refs/heads/wip/execution-1"); got != second.Revision {
		t.Fatalf("remote work-in-progress ref = %q, want %q", got, second.Revision)
	}
}

func TestPushWorkInProgressRejectsUnsafeBranches(t *testing.T) {
	binary, _ := fakeGit(t)
	workspace := Workspace{Repository: t.TempDir()}
	manager := Manager{GitBinary: binary}
	for _, branch := range []string{"", "--upload-pack=payload", "wip/../main", "wip/exec 1"} {
		if _, err := manager.PushWorkInProgress(context.Background(), workspace, branch, nil); err == nil {
			t.Fatalf("PushWorkInProgress() accepted branch %q", branch)
		}
	}
}

func readRemoteRevision(t *testing.T, remote, reference string) string {
	t.Helper()
	command := exec.Command("git", "rev-parse", "--verify", "--quiet", reference)
	command.Dir = remote
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// TestRecordedWorkInProgressSurvivesAPreparationThatMovesTheBranch is what is
// left of this feature's answer to #136 now that #136 is fixed.
//
// The original test asserted that *every* preparation rewound the execution
// branch to the base revision, which is no longer true: a preparation now
// re-creates the branch from its own tip, and a failed run's commit sitting
// there is exactly what the next execution of the job inherits (see
// TestPrepareResumesAJobFromThePreviousExecutionsWork). The anchor still earns
// its keep in the case the branch cannot cover: when the branch has been
// published elsewhere, the preparation resets the local branch onto the
// published tip, and the commit of a failed run is then reachable only through
// a reference outside refs/heads. Real git, because that is where the guarantee
// lives.
func TestRecordedWorkInProgressSurvivesAPreparationThatMovesTheBranch(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	runRealGit(t, root, "init", "-q", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "README.md")
	runRealGit(t, origin, "-c", "user.name=Initial", "-c", "user.email=initial@example.invalid", "commit", "-qm", "initial")

	manager := Manager{DataDirectory: filepath.Join(root, "data"), GitCommitterName: "Moirai Runner", GitCommitterEmail: "runner@example.invalid"}
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}
	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Repository, "partial.go"), []byte("package partial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (7)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	if err := manager.RecordWorkInProgress(context.Background(), workspace, "refs/moirai-wip/execution-1"); err != nil {
		t.Fatalf("RecordWorkInProgress() error = %v", err)
	}

	// Another runner delivers the same job and publishes the branch, so the
	// next preparation here resets the local branch onto the published tip and
	// the work of the failed run is left on no branch at all.
	runRealGit(t, origin, "checkout", "-q", "-b", request.Branch)
	if err := os.WriteFile(filepath.Join(origin, "delivered.go"), []byte("package delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "delivered.go")
	runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
	runRealGit(t, origin, "checkout", "-q", "main")

	if _, err := manager.Prepare(context.Background(), request); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	cache := filepath.Join(root, "data", "repositories", "project-project-1", "repo.git")
	anchored, err := manager.gitOutput(context.Background(), "--git-dir", cache, "rev-parse", "--verify", "refs/moirai-wip/execution-1")
	if err != nil || strings.TrimSpace(statusLine(anchored)) != commit.Revision {
		t.Fatalf("anchored revision = %q, %v, want the work-in-progress commit %q", anchored, err, commit.Revision)
	}
	branch, err := manager.gitOutput(context.Background(), "--git-dir", cache, "rev-parse", "--verify", "refs/heads/agent/issue-7/run-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(statusLine(branch)) == commit.Revision {
		t.Fatal("the execution branch unexpectedly still holds the work-in-progress commit; this test no longer proves the anchor is what saves it")
	}
	files, err := manager.gitOutput(context.Background(), "--git-dir", cache, "show", "--name-only", "--pretty=format:", commit.Revision)
	if err != nil || !strings.Contains(files, "partial.go") {
		t.Fatalf("anchored commit contents = %q, %v", files, err)
	}
}

func TestRecordWorkInProgressRejectsUnsafeReferences(t *testing.T) {
	binary, _ := fakeGit(t)
	workspace := Workspace{Repository: t.TempDir()}
	manager := Manager{GitBinary: binary}
	for _, reference := range []string{"", "wip/execution-1", "refs/../heads/main", "refs/moirai-wip/exec 1", "--upload-pack=payload"} {
		if err := manager.RecordWorkInProgress(context.Background(), workspace, reference); err == nil {
			t.Fatalf("RecordWorkInProgress() accepted reference %q", reference)
		}
	}
}
