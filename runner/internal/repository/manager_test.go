package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
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
		{"-C", cache, "fetch", "--prune", "origin", "main"},
		{"-C", cache, "worktree", "prune"},
		{"-C", cache, "worktree", "add", "-B", "agent/1234/run-a1b2c3", repositoryPath, "main"},
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
	if len(commands) != 6 || !strings.Contains(strings.Join(commands[0], "\n"), "fsck\n--no-dangling") || commands[1][0] != "clone" {
		t.Fatalf("cache recovery commands = %#v", commands)
	}
	if commands[3][len(commands[3])-1] != "prune" || commands[4][3] != "add" {
		t.Fatalf("cache recovery commands = %#v", commands)
	}
	if strings.Join(commands[5], "\n") != strings.Join([]string{"-C", filepath.Join(dataDirectory, "workspaces", "job-job-2", "repository"), "rev-parse", "--git-common-dir"}, "\n") {
		t.Fatalf("cache recovery commands = %#v", commands)
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
		{"-C", localRepository, "fetch", "--prune", "origin", "main"},
		{"-C", localRepository, "worktree", "prune"},
		{"-C", localRepository, "worktree", "add", "-B", "agent/1234/run-a1b2c3", repositoryPath, "origin/main"},
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
	dataDirectory := t.TempDir()
	manager := Manager{DataDirectory: dataDirectory, GitBinary: binary}

	if _, err := manager.Prepare(context.Background(), PrepareRequest{
		ProjectID:     "project-1",
		JobID:         "job-2",
		RepositoryURL: "https://github.com/owner/repository.git",
		DefaultBranch: "main",
		Branch:        "agent/1234/run-a1b2c3",
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
		networked := verb == "clone" || verb == "fetch"
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
	if authenticated != 2 {
		t.Fatalf("authenticated git commands = %d, want clone and fetch: %#v", authenticated, commands)
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
