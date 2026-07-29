package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepositoryLockWaitsForReleaseOrContextCancellation(t *testing.T) {
	manager := Manager{DataDirectory: t.TempDir()}
	root, err := manager.dataDirectory()
	if err != nil {
		t.Fatal(err)
	}
	request := PrepareRequest{ProjectID: "project-1"}
	release, err := manager.lockRepository(context.Background(), root, repositoryLockKey(request))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := manager.lockRepository(ctx, root, repositoryLockKey(request)); err == nil {
		t.Fatal("lockRepository() acquired an already-held repository lock")
	}
	release()
	secondRelease, err := manager.lockRepository(context.Background(), root, repositoryLockKey(request))
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}

func TestPrepareCreatesManagedCloneWorktreeAndTaskDirectory(t *testing.T) {
	binary, recorded := fakeGit(t)
	dataDirectory := t.TempDir()
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	workspace, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:     "project-1",
		JobID:         "job-2",
		RepositoryURL: "https://github.example/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/1234/run-a1b2c3",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !strings.HasPrefix(workspace.Root, dataDirectory+string(filepath.Separator)) {
		t.Fatalf("workspace root = %q outside %q", workspace.Root, dataDirectory)
	}
	if info, err := os.Stat(workspace.Loop); err != nil || !info.IsDir() {
		t.Fatalf("workspace task directory = %q, %v", workspace.Loop, err)
	}
	if workspace.Loop != filepath.Join(workspace.Repository, ".loop") {
		t.Fatalf("workspace task directory = %q, want repository-local .loop", workspace.Loop)
	}

	arguments := readGitCommands(t, recorded)
	cache := filepath.Join(dataDirectory, "repositories", "project-project-1", "repo.git")
	repositoryPath := filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository")
	want := [][]string{
		{"clone", "--mirror", "--", "https://github.example/owner/repository.git", cache},
		{"-C", cache, "worktree", "prune"},
		{"-C", cache, "fetch", "--prune", "origin", "main"},
		{"-C", cache, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", cache, "ls-remote", "--heads", "origin", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", cache, "worktree", "add", "-B", "agent/1234/run-a1b2c3", repositoryPath, "refs/heads/main"},
		{"-C", repositoryPath, "rev-parse", "--git-common-dir"},
	}
	if len(arguments) != len(want) {
		t.Fatalf("git command count = %d, want %d: %#v", len(arguments), len(want), arguments)
	}
	for index := range want {
		if strings.Join(arguments[index], "\n") != strings.Join(want[index], "\n") {
			t.Fatalf("command %d = %#v, want %#v", index, arguments[index], want[index])
		}
	}
}

func TestPrepareChecksExistingManagedCacheAndReclonesCorruption(t *testing.T) {
	binary, recorded := fakeGit(t)
	dataDirectory := t.TempDir()
	cache := filepath.Join(dataDirectory, "repositories", "project-project-1", "repo.git")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_GIT_FAIL_FSCK", "1")
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}
	_, err := manager.Prepare(context.Background(), PrepareRequest{ProjectID: "project-1", JobID: "job-2", RepositoryURL: "https://github.example/owner/repository.git", DefaultBranch: "main", Branch: "agent/1234/run-a1b2c3"})
	if err != nil {
		t.Fatal(err)
	}
	commands := readGitCommands(t, recorded)
	repositoryPath := filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository")
	want := [][]string{
		{"-C", cache, "fsck", "--no-dangling"},
		{"clone", "--mirror", "--", "https://github.example/owner/repository.git", cache},
		{"-C", cache, "worktree", "prune"},
		{"-C", cache, "fetch", "--prune", "origin", "main"},
		{"-C", cache, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", cache, "ls-remote", "--heads", "origin", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", cache, "worktree", "add", "-B", "agent/1234/run-a1b2c3", repositoryPath, "refs/heads/main"},
		{"-C", repositoryPath, "rev-parse", "--git-common-dir"},
	}
	if len(commands) != len(want) {
		t.Fatalf("cache recovery command count = %d, want %d: %#v", len(commands), len(want), commands)
	}
	for index := range want {
		if strings.Join(commands[index], "\n") != strings.Join(want[index], "\n") {
			t.Fatalf("cache recovery command %d = %#v, want %#v", index, commands[index], want[index])
		}
	}
}

func TestPrepareCreatesWorktreeFromExistingLocalPath(t *testing.T) {
	binary, recorded := fakeGit(t)
	dataDirectory := t.TempDir()
	localRepository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(localRepository, 0o700); err != nil {
		t.Fatalf("create local repository: %v", err)
	}
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	workspace, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:           "project-1",
		JobID:               "job-2",
		RepositoryMode:      RepositoryModeExistingPath,
		LocalRepositoryPath: localRepository,
		DefaultBranch:       "main",
		Branch:              "agent/1234/run-a1b2c3",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !strings.HasPrefix(workspace.Root, dataDirectory+string(filepath.Separator)) {
		t.Fatalf("workspace root = %q outside %q", workspace.Root, dataDirectory)
	}
	if info, err := os.Stat(workspace.Loop); err != nil || !info.IsDir() {
		t.Fatalf("workspace task directory = %q, %v", workspace.Loop, err)
	}
	if workspace.Loop != filepath.Join(workspace.Repository, ".loop") {
		t.Fatalf("workspace task directory = %q, want repository-local .loop", workspace.Loop)
	}

	arguments := readGitCommands(t, recorded)
	repositoryPath := filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository")
	want := [][]string{
		{"-C", localRepository, "worktree", "prune"},
		{"-C", localRepository, "fetch", "--prune", "origin", "main"},
		{"-C", localRepository, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", localRepository, "ls-remote", "--heads", "origin", "refs/heads/agent/1234/run-a1b2c3"},
		{"-C", localRepository, "worktree", "add", "-B", "agent/1234/run-a1b2c3", repositoryPath, "refs/remotes/origin/main"},
		{"-C", repositoryPath, "rev-parse", "--git-common-dir"},
	}
	if len(arguments) != len(want) {
		t.Fatalf("git command count = %d, want %d: %#v", len(arguments), len(want), arguments)
	}
	for index := range want {
		if strings.Join(arguments[index], "\n") != strings.Join(want[index], "\n") {
			t.Fatalf("command %d = %#v, want %#v", index, arguments[index], want[index])
		}
	}
}

// TestPrepareStartsFromThePublishedExecutionBranch pins the shape of the
// commands that resume a job whose branch another runner already published: the
// branch is fetched by name and named as the start point, so "worktree add -B"
// re-creates it where the remote has it rather than on the default branch.
func TestPrepareStartsFromThePublishedExecutionBranch(t *testing.T) {
	binary, recorded := fakeGit(t)
	t.Setenv("LOOP_GIT_REMOTE_BRANCH", "1")
	dataDirectory := t.TempDir()
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	if _, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:     "project-1",
		JobID:         "job-2",
		RepositoryURL: "https://github.example/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/issue-7/run-1",
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	arguments := readGitCommands(t, recorded)
	cache := filepath.Join(dataDirectory, "repositories", "project-project-1", "repo.git")
	repositoryPath := filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository")
	want := [][]string{
		{"clone", "--mirror", "--", "https://github.example/owner/repository.git", cache},
		{"-C", cache, "worktree", "prune"},
		{"-C", cache, "fetch", "--prune", "origin", "main"},
		{"-C", cache, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/agent/issue-7/run-1"},
		{"-C", cache, "ls-remote", "--heads", "origin", "refs/heads/agent/issue-7/run-1"},
		{"-C", cache, "fetch", "--refmap=", "origin", "+refs/heads/agent/issue-7/run-1:refs/moirai-remote/agent/issue-7/run-1"},
		{"-C", cache, "worktree", "add", "-B", "agent/issue-7/run-1", repositoryPath, "refs/moirai-remote/agent/issue-7/run-1"},
		{"-C", repositoryPath, "rev-parse", "--git-common-dir"},
	}
	if len(arguments) != len(want) {
		t.Fatalf("git command count = %d, want %d: %#v", len(arguments), len(want), arguments)
	}
	for index := range want {
		if strings.Join(arguments[index], "\n") != strings.Join(want[index], "\n") {
			t.Fatalf("command %d = %#v, want %#v", index, arguments[index], want[index])
		}
	}
}

func TestPrepareAuthenticatesManagedCloneAndFetchWithResolvedEnvironment(t *testing.T) {
	binary, recorded := fakeGit(t)
	// The remote holds the execution branch, so every networked command a
	// resumed job issues — including the two that #136 added — is covered.
	t.Setenv("LOOP_GIT_REMOTE_BRANCH", "1")
	dataDirectory := t.TempDir()
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	if _, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:     "project-1",
		JobID:         "job-2",
		RepositoryURL: "https://github.com/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/issue-7/run-1",
		Environment:   map[string]string{"GITHUB_TOKEN": "token-value"},
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	commands := readGitCommands(t, recorded)
	environments := readGitEnvironments(t, recorded)
	if len(commands) != len(environments) {
		t.Fatalf("recorded %d commands and %d environments", len(commands), len(environments))
	}
	credential := base64.StdEncoding.EncodeToString([]byte("x-access-token:token-value"))
	authenticated := 0
	for index, command := range commands {
		verb := command[0]
		if verb == "-C" {
			verb = command[2]
		}
		networked := verb == "clone" || verb == "fetch" || verb == "ls-remote"
		hasHeader := gitEnvironmentContains(environments[index], "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader")
		hasValue := gitEnvironmentContains(environments[index], "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+credential)
		hasToken := gitEnvironmentContains(environments[index], "GITHUB_TOKEN=token-value")
		if networked != (hasHeader && hasValue && hasToken) {
			t.Fatalf("command %d (%#v) credential environment = %#v", index, command, environments[index])
		}
		if networked {
			authenticated++
		}
	}
	if authenticated != 4 {
		t.Fatalf("authenticated git commands = %d, want clone, default-branch fetch, ls-remote and execution-branch fetch: %#v", authenticated, commands)
	}
}

func TestPrepareRejectsUnsafeResolvedEnvironment(t *testing.T) {
	binary, _ := fakeGit(t)
	manager := Manager{DataDirectory: t.TempDir(), GitBinary: binary}
	_, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:     "project-1",
		JobID:         "job-2",
		RepositoryURL: "https://github.com/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/1234/run-a1b2c3",
		Environment:   map[string]string{"GITHUB_TOKEN": "token\nGIT_CONFIG_COUNT=0"},
	})
	if err == nil || !strings.Contains(err.Error(), "git credential environment is invalid") {
		t.Fatalf("Prepare() error = %v, want invalid credential environment", err)
	}
}

func TestCleanupExistingRemovesOnlyManagedWorkspace(t *testing.T) {
	binary, recorded := fakeGit(t)
	dataDirectory := t.TempDir()
	localRepository := filepath.Join(t.TempDir(), "repository")
	workspace := filepath.Join(dataDirectory, "workspaces", "job-job-2")
	if err := os.MkdirAll(localRepository, 0o700); err != nil {
		t.Fatalf("create local repository: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "repository"), 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	if err := manager.CleanupExisting(context.Background(), localRepository, "job-2"); err != nil {
		t.Fatalf("CleanupExisting() error = %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after cleanup: %v", err)
	}
	arguments := readGitCommands(t, recorded)
	want := []string{"-C", localRepository, "worktree", "remove", "--force", filepath.Join(workspace, "repository")}
	if len(arguments) != 1 || strings.Join(arguments[0], "\n") != strings.Join(want, "\n") {
		t.Fatalf("cleanup git arguments = %#v, want %#v", arguments, want)
	}
}

func TestCleanupQuarantinesWorkspaceAfterBoundedFailures(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "git")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write failing Git: %v", err)
	}
	dataDirectory := t.TempDir()
	workspace := filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	attempts := 0
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary, CleanupAttempts: 2, Sleep: func(time.Duration) { attempts++ }}
	if err := manager.Cleanup(context.Background(), "project-1", "job-2"); err == nil {
		t.Fatal("Cleanup() succeeded")
	}
	if attempts != 1 {
		t.Fatalf("cleanup retries = %d, want 1", attempts)
	}
	contents, err := os.ReadFile(filepath.Join(dataDirectory, "quarantine", "job-job-2.json"))
	if err != nil {
		t.Fatalf("read quarantine record: %v", err)
	}
	var record struct {
		JobID string `json:"jobId"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(contents, &record); err != nil || record.JobID != "job-2" || record.Error == "" {
		t.Fatalf("quarantine record = %q, err = %v", contents, err)
	}
}

func TestPrepareRejectsUnsafeIdentifiersBranchesAndURLs(t *testing.T) {
	manager := Manager{DataDirectory: t.TempDir()}
	valid := PrepareRequest{
		ProjectID:     "project",
		JobID:         "job",
		RepositoryURL: "https://github.example/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/1/run",
	}
	cases := []PrepareRequest{
		func() PrepareRequest { request := valid; request.ProjectID = "../outside"; return request }(),
		func() PrepareRequest { request := valid; request.JobID = "job/name"; return request }(),
		func() PrepareRequest { request := valid; request.RepositoryURL = "--upload-pack=evil"; return request }(),
		func() PrepareRequest { request := valid; request.Branch = "agent/../run"; return request }(),
		func() PrepareRequest {
			request := valid
			request.DefaultBranch = "main\n--upload-pack=evil"
			return request
		}(),
		func() PrepareRequest {
			request := valid
			request.RepositoryMode = RepositoryModeExistingPath
			request.RepositoryURL = ""
			request.LocalRepositoryPath = "relative/repository"
			return request
		}(),
		func() PrepareRequest {
			request := valid
			request.RepositoryMode = RepositoryModeExistingPath
			request.LocalRepositoryPath = "/repository\n--upload-pack=evil"
			return request
		}(),
		func() PrepareRequest {
			request := valid
			request.RepositoryMode = "unsupported"
			return request
		}(),
	}
	for _, request := range cases {
		if _, err := manager.Prepare(context.Background(), request); err == nil {
			t.Fatalf("Prepare(%+v) succeeded for unsafe request", request)
		}
	}
}

func TestCleanupRemovesOnlyManagedWorkspace(t *testing.T) {
	binary, recorded := fakeGit(t)
	dataDirectory := t.TempDir()
	workspace := filepath.Join(dataDirectory, "workspaces", "job-job-2")
	if err := os.MkdirAll(filepath.Join(workspace, "repository"), 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	if err := manager.Cleanup(context.Background(), "project-1", "job-2"); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after cleanup: %v", err)
	}
	arguments := readGitCommands(t, recorded)
	if len(arguments) != 1 || !strings.Contains(strings.Join(arguments[0], "\n"), "worktree\nremove\n--force") {
		t.Fatalf("cleanup git arguments = %#v", arguments)
	}
}

func fakeGit(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	recorded := filepath.Join(directory, "arguments")
	binary := filepath.Join(directory, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$LOOP_GIT_ARGS\"\nprintf '\\036' >> \"$LOOP_GIT_ARGS\"\nenv >> \"$LOOP_GIT_ARGS.env\"\nprintf '\\036' >> \"$LOOP_GIT_ARGS.env\"\nfor argument in \"$@\"; do if [ \"$argument\" = fsck ] && [ \"$LOOP_GIT_FAIL_FSCK\" = 1 ]; then exit 1; fi; if [ \"$argument\" = status ] && [ \"$LOOP_GIT_STATUS\" = 1 ]; then printf ' M file\\000'; fi; if [ \"$argument\" = ls-remote ] && [ \"$LOOP_GIT_REMOTE_BRANCH\" = 1 ]; then printf 'revision\\trefs/heads/agent/issue-7/run-1\\n'; fi; if [ \"$argument\" = --git-common-dir ]; then printf '.git\\n'; fi; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("LOOP_GIT_ARGS", recorded)
	return binary, recorded
}

// readGitEnvironments returns the environment of each recorded git invocation,
// in the same order as readGitCommands.
func readGitEnvironments(t *testing.T, path string) [][]string {
	t.Helper()
	return readGitCommands(t, path+".env")
}

func gitEnvironmentContains(environment []string, entry string) bool {
	for _, value := range environment {
		if value == entry {
			return true
		}
	}
	return false
}

func readGitCommands(t *testing.T, path string) [][]string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded Git commands: %v", err)
	}
	var commands [][]string
	for _, command := range strings.Split(string(contents), "\036") {
		command = strings.TrimSuffix(command, "\n")
		if command == "" {
			continue
		}
		commands = append(commands, strings.Split(command, "\n"))
	}
	return commands
}

// TestRetainedWorkspaceDoesNotBlockThePreparationOfTheNextExecution exercises
// real Git rather than the recording stub, because the hazard is Git's own
// rule: a branch checked out by one worktree cannot be re-created in another.
// The orchestrator reuses a single branch name for every execution of a
// workflow, so a retained failed workspace would otherwise make that workflow's
// next attempt fail in Prepare.
func TestRetainedWorkspaceDoesNotBlockThePreparationOfTheNextExecution(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(workspace.Loop, "task-packet.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "wip(failed): developer: A task (7)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	committed, err := manager.gitOutput(context.Background(), "-C", workspace.Repository, "show", "--name-only", "--pretty=format:", "HEAD")
	if err != nil || !strings.Contains(committed, "partial.go") || strings.Contains(committed, ".loop") {
		t.Fatalf("committed files = %q, %v", committed, err)
	}

	// Retention keeps this workspace, so its branch has to be released.
	if err := manager.ReleaseBranch(context.Background(), workspace); err != nil {
		t.Fatalf("ReleaseBranch() error = %v", err)
	}

	retry := request
	retry.JobID = "job-2"
	if _, err := manager.Prepare(context.Background(), retry); err != nil {
		t.Fatalf("Prepare() after retention error = %v", err)
	}
	revision, err := manager.gitOutput(context.Background(), "-C", workspace.Repository, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(statusLine(revision)) != commit.Revision {
		t.Fatalf("retained workspace HEAD = %q, %v, want the work-in-progress commit %q", revision, err, commit.Revision)
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, "partial.go")); err != nil {
		t.Fatalf("retained workspace lost the agent's files: %v", err)
	}
}

// The tests below run against real Git because #136 was a defect in Git's
// own semantics: "git worktree add -B <branch> <path> <default-branch>" is
// create-or-*reset*, so it silently rewound the job's branch onto the default
// branch on every execution. A recording stub agrees with whatever is asserted
// about the arguments; only Git decides what the workspace then contains.

// TestPrepareStartsAJobWithoutAnExecutionBranchFromTheDefaultBranch is the
// second acceptance criterion of #136: a first execution has no work to
// inherit, so it starts from the default branch.
func TestPrepareStartsAJobWithoutAnExecutionBranchFromTheDefaultBranch(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	// Another job's branch exists on the remote and must not be mistaken for
	// this job's.
	runRealGit(t, origin, "branch", "agent/issue-7/other-job")

	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}
	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got, want := workspaceRevision(t, manager, workspace), realGitRevision(t, origin, "refs/heads/main"); got != want {
		t.Fatalf("first execution HEAD = %q, want the default branch %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, "README.md")); err != nil {
		t.Fatalf("first execution workspace is missing the default branch's files: %v", err)
	}
}

// TestPrepareResumesAJobFromThePreviousExecutionsWork is the first acceptance
// criterion of #136: the execution that follows a developer execution — the
// local pipeline, which decides whether the work is complete — has to run
// against that developer's tree.
func TestPrepareResumesAJobFromThePreviousExecutionsWork(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}

	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Repository, "feature.go"), []byte("package feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "developer: implement a feature (7)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	// The runner removes the workspace once an execution ends; the branch, not
	// the directory, is what the next execution of the job inherits. Nothing is
	// pushed here: the developer role is the only one the orchestrator grants
	// `mayPush`, and a project may have no writable remote at all.
	if err := manager.Cleanup(context.Background(), request.ProjectID, request.JobID); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	pipeline, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() for the following execution error = %v", err)
	}
	if got := workspaceRevision(t, manager, pipeline); got != commit.Revision {
		t.Fatalf("following execution HEAD = %q, want the developer's commit %q", got, commit.Revision)
	}
	if got := workspaceRevision(t, manager, pipeline); got == realGitRevision(t, origin, "refs/heads/main") {
		t.Fatal("following execution was reset to the default branch, so its pipeline would validate nothing")
	}
	contents, err := os.ReadFile(filepath.Join(pipeline.Repository, "feature.go"))
	if err != nil || string(contents) != "package feature\n" {
		t.Fatalf("following execution workspace file = %q, %v", contents, err)
	}
}

// TestPrepareResumesAJobFromTheBranchPublishedByAnotherRunner covers the case
// the local branch cannot: executions of one job are leased independently, so
// the work of the previous one may only exist on the remote.
func TestPrepareResumesAJobFromTheBranchPublishedByAnotherRunner(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}

	if _, err := manager.Prepare(context.Background(), request); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := manager.Cleanup(context.Background(), request.ProjectID, request.JobID); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	// Another runner executed this job and published the branch. This runner's
	// own copy of it still points at the default branch.
	runRealGit(t, origin, "checkout", "-q", "-b", request.Branch)
	if err := os.WriteFile(filepath.Join(origin, "delivered.go"), []byte("package delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "delivered.go")
	runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
	published := realGitRevision(t, origin, "refs/heads/"+request.Branch)
	runRealGit(t, origin, "checkout", "-q", "main")

	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() after publication error = %v", err)
	}
	if got := workspaceRevision(t, manager, workspace); got != published {
		t.Fatalf("resumed HEAD = %q, want the published execution branch %q", got, published)
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, "delivered.go")); err != nil {
		t.Fatalf("resumed workspace is missing the published work: %v", err)
	}
}

// TestPrepareResumesAnExistingPathJobFromItsExecutionBranch runs the same two
// criteria against the existing_path mode, whose source repository is a working
// checkout rather than a mirror: the branch it starts from is a remote-tracking
// reference, and its own refs/heads belong to whoever works in that checkout.
func TestPrepareResumesAnExistingPathJobFromItsExecutionBranch(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	checkout := filepath.Join(root, "checkout")
	runRealGit(t, root, "clone", "-q", origin, checkout)
	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryMode: RepositoryModeExistingPath, LocalRepositoryPath: checkout, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}

	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got, want := workspaceRevision(t, manager, workspace), realGitRevision(t, origin, "refs/heads/main"); got != want {
		t.Fatalf("first execution HEAD = %q, want the default branch %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(workspace.Repository, "feature.go"), []byte("package feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := manager.Commit(context.Background(), workspace, "developer: implement a feature (7)")
	if err != nil || !commit.Committed {
		t.Fatalf("Commit() = %#v, %v", commit, err)
	}
	if err := manager.CleanupExisting(context.Background(), checkout, request.JobID); err != nil {
		t.Fatalf("CleanupExisting() error = %v", err)
	}

	resumed, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() for the following execution error = %v", err)
	}
	if got := workspaceRevision(t, manager, resumed); got != commit.Revision {
		t.Fatalf("following execution HEAD = %q, want the developer's commit %q", got, commit.Revision)
	}
	if _, err := os.Stat(filepath.Join(resumed.Repository, "feature.go")); err != nil {
		t.Fatalf("following execution workspace lost the developer's file: %v", err)
	}

	// The published branch stays authoritative here too: a checkout's own
	// refs/heads must not outrank what the remote holds.
	runRealGit(t, origin, "checkout", "-q", "-b", request.Branch)
	if err := os.WriteFile(filepath.Join(origin, "delivered.go"), []byte("package delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "delivered.go")
	runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
	published := realGitRevision(t, origin, "refs/heads/"+request.Branch)
	runRealGit(t, origin, "checkout", "-q", "main")
	if err := manager.CleanupExisting(context.Background(), checkout, request.JobID); err != nil {
		t.Fatalf("CleanupExisting() error = %v", err)
	}

	republished, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() after publication error = %v", err)
	}
	if got := workspaceRevision(t, manager, republished); got != published {
		t.Fatalf("resumed HEAD = %q, want the published execution branch %q", got, published)
	}
}

// TestPrepareKeepsWorkBuiltOnTopOfThePublishedExecutionBranch is the case an
// unconditional fetch of the published branch destroys, and it destroys it
// silently. Only `developer` is granted `mayPush`; a `repairer` that *completes*
// commits to the execution branch, pushes nothing, and writes no
// refs/moirai-wip anchor either — #100 anchors the work of runs that fail or
// block. Re-creating the branch from the published tip would therefore leave
// that commit on no reference at all, and the pipeline execution after it would
// validate the pre-repair tree.
func TestPrepareKeepsWorkBuiltOnTopOfThePublishedExecutionBranch(t *testing.T) {
	for _, mode := range []RepositoryMode{RepositoryModeManagedClone, RepositoryModeExistingPath} {
		t.Run(string(mode), func(t *testing.T) {
			root := t.TempDir()
			origin := newOriginRepository(t, root)
			// A developer execution delivered and published the branch.
			branch := "agent/issue-7/run-1"
			runRealGit(t, origin, "checkout", "-q", "-b", branch)
			if err := os.WriteFile(filepath.Join(origin, "feature.go"), []byte("package feature\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRealGit(t, origin, "add", "feature.go")
			runRealGit(t, origin, "-c", "user.name=Dev", "-c", "user.email=dev@example.invalid", "commit", "-qm", "developer: implement")
			published := realGitRevision(t, origin, "refs/heads/"+branch)
			runRealGit(t, origin, "checkout", "-q", "main")

			manager := newRealGitManager(root)
			request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", DefaultBranch: "main", Branch: branch}
			cleanup := func() {
				t.Helper()
				if err := manager.Cleanup(context.Background(), request.ProjectID, request.JobID); err != nil {
					t.Fatalf("Cleanup() error = %v", err)
				}
			}
			if mode == RepositoryModeExistingPath {
				checkout := filepath.Join(root, "checkout")
				runRealGit(t, root, "clone", "-q", origin, checkout)
				request.RepositoryMode = mode
				request.LocalRepositoryPath = checkout
				cleanup = func() {
					t.Helper()
					if err := manager.CleanupExisting(context.Background(), checkout, request.JobID); err != nil {
						t.Fatalf("CleanupExisting() error = %v", err)
					}
				}
			} else {
				request.RepositoryURL = origin
			}

			workspace, err := manager.Prepare(context.Background(), request)
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			if got := workspaceRevision(t, manager, workspace); got != published {
				t.Fatalf("HEAD = %q, want the published branch %q", got, published)
			}
			// A repairer completes: it commits on the execution branch and, with
			// no mayPush, publishes nothing.
			if err := os.WriteFile(filepath.Join(workspace.Repository, "repair.go"), []byte("package repair\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			repair, err := manager.Commit(context.Background(), workspace, "repairer: fix the failing check (7)")
			if err != nil || !repair.Committed {
				t.Fatalf("Commit() = %#v, %v", repair, err)
			}
			cleanup()

			pipeline, err := manager.Prepare(context.Background(), request)
			if err != nil {
				t.Fatalf("Prepare() for the following execution error = %v", err)
			}
			if got := workspaceRevision(t, manager, pipeline); got != repair.Revision {
				t.Fatalf("following execution HEAD = %q, want the repairer's commit %q (published tip is %q)", got, repair.Revision, published)
			}
			for _, name := range []string{"feature.go", "repair.go"} {
				if _, err := os.Stat(filepath.Join(pipeline.Repository, name)); err != nil {
					t.Fatalf("following execution workspace is missing %s: %v", name, err)
				}
			}

			// The published branch still wins when it holds work this runner
			// does not: another runner delivering on top of the same base makes
			// the two diverge, and the tip every runner agrees on decides.
			runRealGit(t, origin, "checkout", "-q", branch)
			if err := os.WriteFile(filepath.Join(origin, "elsewhere.go"), []byte("package elsewhere\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runRealGit(t, origin, "add", "elsewhere.go")
			runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
			diverged := realGitRevision(t, origin, "refs/heads/"+branch)
			runRealGit(t, origin, "checkout", "-q", "main")
			cleanup()

			resumed, err := manager.Prepare(context.Background(), request)
			if err != nil {
				t.Fatalf("Prepare() after divergence error = %v", err)
			}
			if got := workspaceRevision(t, manager, resumed); got != diverged {
				t.Fatalf("HEAD after divergence = %q, want the published tip %q", got, diverged)
			}
		})
	}
}

// TestPrepareBaseRevisionLeavesTheExecutionBranchWhereItIs pins "--refmap=" on
// the execution-branch fetch. A managed cache is a mirror, and its configured
// "+refs/*:refs/*" force-updates refs/heads/<branch> on any fetch that names
// that branch, whatever destination the refspec itself gives. Resolving the
// base revision must move no branch: a preparation that fails after the fetch
// would otherwise leave an unpushed commit reachable through the reflog alone.
func TestPrepareBaseRevisionLeavesTheExecutionBranchWhereItIs(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	branch := "agent/issue-7/run-1"
	runRealGit(t, origin, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(origin, "feature.go"), []byte("package feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "feature.go")
	runRealGit(t, origin, "-c", "user.name=Dev", "-c", "user.email=dev@example.invalid", "commit", "-qm", "developer: implement")
	runRealGit(t, origin, "checkout", "-q", "main")

	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: branch}
	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Repository, "repair.go"), []byte("package repair\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repair, err := manager.Commit(context.Background(), workspace, "repairer: fix the failing check (7)")
	if err != nil || !repair.Committed {
		t.Fatalf("Commit() = %#v, %v", repair, err)
	}
	if err := manager.Cleanup(context.Background(), request.ProjectID, request.JobID); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	cache := filepath.Join(root, "data", "repositories", "project-project-1", "repo.git")
	baseRevision, err := manager.prepareBaseRevision(context.Background(), cache, request, nil)
	if err != nil {
		t.Fatalf("prepareBaseRevision() error = %v", err)
	}
	if baseRevision != repair.Revision {
		t.Fatalf("base revision = %q, want the unpushed commit %q", baseRevision, repair.Revision)
	}
	if got := realGitRevision(t, cache, "refs/heads/"+branch); got != repair.Revision {
		t.Fatalf("resolving the base revision moved the execution branch to %q, want it left at %q", got, repair.Revision)
	}
}

// TestPrepareResumesAJobWhosePreviousWorkspaceWasNeverCleanedUp pins the
// ordering: the workspace of a crashed or retained execution leaves a worktree
// registration claiming the execution branch, and Git refuses to fetch into a
// branch a worktree claims — even one whose directory is gone. Pruning has to
// happen before the branch is resolved, not merely before it is checked out.
func TestPrepareResumesAJobWhosePreviousWorkspaceWasNeverCleanedUp(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	manager := newRealGitManager(root)
	request := PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryURL: origin, DefaultBranch: "main", Branch: "agent/issue-7/run-1"}

	if _, err := manager.Prepare(context.Background(), request); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	// No Cleanup: the registration of that worktree survives into the next
	// preparation, which removes only its directory.
	runRealGit(t, origin, "checkout", "-q", "-b", request.Branch)
	if err := os.WriteFile(filepath.Join(origin, "delivered.go"), []byte("package delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "delivered.go")
	runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
	published := realGitRevision(t, origin, "refs/heads/"+request.Branch)
	runRealGit(t, origin, "checkout", "-q", "main")

	workspace, err := manager.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() over a stale worktree registration error = %v", err)
	}
	if got := workspaceRevision(t, manager, workspace); got != published {
		t.Fatalf("resumed HEAD = %q, want the published execution branch %q", got, published)
	}
}

// TestPrepareResumesAnExistingPathJobWhoseCheckoutTracksOneBranch is why the
// execution branch is fetched with an explicit destination: a checkout cloned
// with --single-branch has no configured refspec that would bring that branch
// into refs/remotes/origin/*, so a fetch that left the destination to Git would
// update nothing and the workspace would silently start from the default
// branch — the defect this issue is about.
func TestPrepareResumesAnExistingPathJobWhoseCheckoutTracksOneBranch(t *testing.T) {
	root := t.TempDir()
	origin := newOriginRepository(t, root)
	branch := "agent/issue-7/run-1"
	runRealGit(t, origin, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(origin, "delivered.go"), []byte("package delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "delivered.go")
	runRealGit(t, origin, "-c", "user.name=Other", "-c", "user.email=other@example.invalid", "commit", "-qm", "delivered elsewhere")
	published := realGitRevision(t, origin, "refs/heads/"+branch)
	runRealGit(t, origin, "checkout", "-q", "main")

	checkout := filepath.Join(root, "checkout")
	runRealGit(t, root, "clone", "-q", "--single-branch", "--branch", "main", origin, checkout)
	manager := newRealGitManager(root)
	workspace, err := manager.Prepare(context.Background(), PrepareRequest{ProjectID: "project-1", JobID: "job-1", RepositoryMode: RepositoryModeExistingPath, LocalRepositoryPath: checkout, DefaultBranch: "main", Branch: branch})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got := workspaceRevision(t, manager, workspace); got != published {
		t.Fatalf("resumed HEAD = %q, want the published execution branch %q", got, published)
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, "delivered.go")); err != nil {
		t.Fatalf("resumed workspace is missing the published work: %v", err)
	}
}

func newOriginRepository(t *testing.T, root string) string {
	t.Helper()
	origin := filepath.Join(root, "origin")
	runRealGit(t, root, "init", "-q", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRealGit(t, origin, "add", "README.md")
	runRealGit(t, origin, "-c", "user.name=Initial", "-c", "user.email=initial@example.invalid", "commit", "-qm", "initial")
	return origin
}

func newRealGitManager(root string) Manager {
	return Manager{DataDirectory: filepath.Join(root, "data"), GitCommitterName: "Moirai Runner", GitCommitterEmail: "runner@example.invalid"}
}

func workspaceRevision(t *testing.T, manager Manager, workspace Workspace) string {
	t.Helper()
	revision, err := manager.gitOutput(context.Background(), "-C", workspace.Repository, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("read workspace revision: %v", err)
	}
	return strings.TrimSpace(statusLine(revision))
}

func realGitRevision(t *testing.T, repository, reference string) string {
	t.Helper()
	command := exec.Command("git", "rev-parse", "--verify", reference)
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		t.Fatalf("read %s in %s: %v", reference, repository, err)
	}
	return strings.TrimSpace(string(output))
}

// TestWithPruneCauseKeepsBothTheFailureAndItsExplanation covers the diagnostic
// a swallowed prune error used to cost: Git reports the worktree still claiming
// the branch, never why the claim outlived the workspace.
func TestWithPruneCauseKeepsBothTheFailureAndItsExplanation(t *testing.T) {
	failure := errors.New("create worktree: is already used by worktree")
	if got := withPruneCause(failure, nil); got != failure {
		t.Fatalf("withPruneCause() = %v, want the failure unchanged when the prune succeeded", got)
	}
	decorated := withPruneCause(failure, errors.New("git worktree: permission denied"))
	if !errors.Is(decorated, failure) {
		t.Fatalf("withPruneCause() = %v, want the original failure to stay unwrappable", decorated)
	}
	if !strings.Contains(decorated.Error(), "permission denied") || !strings.Contains(decorated.Error(), "is already used by worktree") {
		t.Fatalf("withPruneCause() = %q, want both the failure and the prune error", decorated)
	}
}

func TestReleaseBranchRejectsWorkspacesOutsideTheDataDirectory(t *testing.T) {
	manager := Manager{DataDirectory: t.TempDir()}
	escaped := t.TempDir()
	if err := manager.ReleaseBranch(context.Background(), Workspace{Root: escaped, Repository: filepath.Join(escaped, "repository")}); err == nil {
		t.Fatal("ReleaseBranch() accepted a workspace outside the data directory")
	}
}
