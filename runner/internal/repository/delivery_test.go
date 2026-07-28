package repository

import (
	"context"
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
	if !strings.Contains(strings.Join(joined, "\n"), "add -A -- . :!.loop :!.loop/**") || !strings.Contains(strings.Join(joined, "\n"), "commit -m loop: implement issue 7") || !strings.Contains(strings.Join(joined, "\n"), "push --set-upstream origin agent/issue-7/run-1") {
		t.Fatalf("delivery commands = %#v", commands)
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
	pushed, err := manager.Push(context.Background(), workspace, "main", nil)
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
