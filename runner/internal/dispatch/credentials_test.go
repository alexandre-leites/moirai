package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

func credentialWorkspace(t *testing.T) repository.Workspace {
	t.Helper()
	root := t.TempDir()
	checkout := filepath.Join(root, "repository")
	if err := os.MkdirAll(filepath.Join(checkout, ".loop"), 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return repository.Workspace{Root: root, Repository: checkout, Loop: filepath.Join(checkout, ".loop")}
}

// The trap the whole file exists for: MinimalEnvironment overrides HOME, so a
// harness reading credentials from ~ finds whatever the runner put there and
// nothing else.
func TestFileDeliveredCredentialLandsUnderTheExecutionHome(t *testing.T) {
	workspace := credentialWorkspace(t)
	references := []taskpacket.EnvironmentRef{
		{Name: "OPENCODE_AUTH", SecretRef: "agent:OPENCODE_AUTH", Path: ".local/share/opencode/auth.json"},
	}

	environment, files, err := prepareExecutionEnvironment(workspace, references, map[string]string{
		"OPENCODE_AUTH": `{"access":"token-value"}`,
	})
	if err != nil {
		t.Fatalf("prepareExecutionEnvironment() error = %v", err)
	}

	home := environment["HOME"]
	if home != filepath.Join(workspace.Root, "home") {
		t.Fatalf("HOME = %q, want the execution home", home)
	}
	expected := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	if got := environment["OPENCODE_AUTH"]; got != expected {
		t.Fatalf("OPENCODE_AUTH = %q, want the credential path %q", got, expected)
	}
	contents, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	if string(contents) != `{"access":"token-value"}` {
		t.Fatalf("credential file = %q", contents)
	}
	info, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("stat credential file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %v, want 0600", info.Mode().Perm())
	}
	if len(files) != 1 || files[0].Name != "OPENCODE_AUTH" {
		t.Fatalf("credential files = %#v", files)
	}
}

// HOME used to be the checkout, so anything a tool wrote to ~ landed in the
// tree the agent was about to commit. A credential there is one `git add -A`
// from the code host.
func TestTheExecutionHomeIsOutsideTheCheckout(t *testing.T) {
	workspace := credentialWorkspace(t)

	environment, _, err := prepareExecutionEnvironment(workspace, nil, map[string]string{})
	if err != nil {
		t.Fatalf("prepareExecutionEnvironment() error = %v", err)
	}

	relative, err := filepath.Rel(workspace.Repository, environment["HOME"])
	if err == nil && !strings.HasPrefix(relative, "..") {
		t.Fatalf("HOME %q is inside the checkout %q", environment["HOME"], workspace.Repository)
	}
}

func TestAnEnvironmentDeliveredCredentialIsNotWrittenToDisk(t *testing.T) {
	workspace := credentialWorkspace(t)
	references := []taskpacket.EnvironmentRef{{Name: "OPENROUTER_API_KEY", SecretRef: "agent:OPENROUTER_API_KEY"}}

	environment, files, err := prepareExecutionEnvironment(workspace, references, map[string]string{
		"OPENROUTER_API_KEY": "sk-or-value",
	})
	if err != nil {
		t.Fatalf("prepareExecutionEnvironment() error = %v", err)
	}

	if environment["OPENROUTER_API_KEY"] != "sk-or-value" {
		t.Fatalf("OPENROUTER_API_KEY = %q, want the value itself", environment["OPENROUTER_API_KEY"])
	}
	if len(files) != 0 {
		t.Fatalf("credential files = %#v, want none", files)
	}
}

func TestTheRepositoryEnvironmentIsNotRewrittenByFileDelivery(t *testing.T) {
	workspace := credentialWorkspace(t)
	references := []taskpacket.EnvironmentRef{
		{Name: "OPENCODE_AUTH", SecretRef: "agent:OPENCODE_AUTH", Path: "auth.json"},
	}
	repositoryEnvironment := map[string]string{"OPENCODE_AUTH": "secret", "GITHUB_TOKEN": "ghp_token"}

	environment, _, err := prepareExecutionEnvironment(workspace, references, repositoryEnvironment)
	if err != nil {
		t.Fatalf("prepareExecutionEnvironment() error = %v", err)
	}

	// git wants GITHUB_TOKEN as a value, and the caller keeps using this map
	// for clone, commit and push after the agent's copy has been rewritten.
	if repositoryEnvironment["OPENCODE_AUTH"] != "secret" {
		t.Fatalf("the repository environment was mutated: %#v", repositoryEnvironment)
	}
	if environment["GITHUB_TOKEN"] != "ghp_token" {
		t.Fatalf("GITHUB_TOKEN = %q", environment["GITHUB_TOKEN"])
	}
}

func TestACredentialPathCannotEscapeTheExecutionHome(t *testing.T) {
	workspace := credentialWorkspace(t)
	home := filepath.Join(workspace.Root, "home")

	for _, declared := range []string{"../escape", "a/../../b", "/etc/passwd"} {
		t.Run(declared, func(t *testing.T) {
			if _, err := credentialFilePath(home, declared); err == nil {
				t.Fatalf("credentialFilePath(%q) accepted a path outside the home", declared)
			}
		})
	}
}

// A retained workspace keeps a failed run's directory for forensics. Without
// this it would keep the credential the run was given along with it.
func TestDiscardRemovesDeliveredCredentialFiles(t *testing.T) {
	workspace := credentialWorkspace(t)
	references := []taskpacket.EnvironmentRef{
		{Name: "OPENCODE_AUTH", SecretRef: "agent:OPENCODE_AUTH", Path: "auth.json"},
	}
	_, files, err := prepareExecutionEnvironment(workspace, references, map[string]string{"OPENCODE_AUTH": "secret"})
	if err != nil {
		t.Fatalf("prepareExecutionEnvironment() error = %v", err)
	}

	if err := discardCredentialFiles(files); err != nil {
		t.Fatalf("discardCredentialFiles() error = %v", err)
	}

	if _, err := os.Stat(files[0].Path); !os.IsNotExist(err) {
		t.Fatalf("credential file still present: %v", err)
	}
	// Removing what is already gone is not a failure: cleanup runs on every
	// path out, including the ones where the workspace was already discarded.
	if err := discardCredentialFiles(files); err != nil {
		t.Fatalf("second discardCredentialFiles() error = %v", err)
	}
}

func TestSecretLiteralsIncludeTheTokensInsideACredentialsDocument(t *testing.T) {
	document := `{"type":"oauth","access":"sk-ant-oat01-averylongaccesstoken","refresh":"sk-ant-ort01-averylongrefreshtoken"}`

	literals := secretLiterals(document)

	for _, want := range []string{
		document,
		"sk-ant-oat01-averylongaccesstoken",
		"sk-ant-ort01-averylongrefreshtoken",
	} {
		found := false
		for _, literal := range literals {
			if literal == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("secretLiterals() = %#v, missing %q", literals, want)
		}
	}
	// "oauth" is a field marker, not a secret. Registering it would redact an
	// ordinary word out of every log line.
	for _, literal := range literals {
		if literal == "oauth" {
			t.Fatalf("secretLiterals() registered a field marker: %#v", literals)
		}
	}
}

func TestSecretLiteralsOfAPlainKeyIsJustTheKey(t *testing.T) {
	if got := secretLiterals("sk-or-v1-plainkey"); len(got) != 1 || got[0] != "sk-or-v1-plainkey" {
		t.Fatalf("secretLiterals() = %#v", got)
	}
}

type recordingStore struct {
	mu     sync.Mutex
	calls  [][2]string
	err    error
	stored chan struct{}
}

func (s *recordingStore) store(_ context.Context, name, value string) error {
	s.mu.Lock()
	s.calls = append(s.calls, [2]string{name, value})
	err := s.err
	s.mu.Unlock()
	if s.stored != nil {
		select {
		case s.stored <- struct{}{}:
		default:
		}
	}
	return err
}

func (s *recordingStore) snapshot() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]string(nil), s.calls...)
}

// The value the runner itself just delivered must never be written straight
// back as though the harness had rotated it.
func TestARotationWatcherIgnoresTheValueItWasGiven(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, []byte(`{"access":"original"}`), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	store := &recordingStore{}
	watcher := newRotationWatcher(
		[]credentialFile{{Name: "OPENCODE_AUTH", Path: path}},
		time.Millisecond, store.store, nil, nil,
	)

	watcher.Start(context.Background())
	watcher.Stop()

	if calls := store.snapshot(); len(calls) != 0 {
		t.Fatalf("store called for an unchanged credential: %#v", calls)
	}
}

// The point of the whole mechanism: a token refreshed inside the execution
// reaches the control plane, so the next execution starts from a live one.
func TestARotationWatcherPersistsARefreshedCredential(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, []byte(`{"access":"original"}`), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	store := &recordingStore{stored: make(chan struct{}, 1)}
	var redacted []string
	watcher := newRotationWatcher(
		[]credentialFile{{Name: "OPENCODE_AUTH", Path: path}},
		time.Millisecond, store.store,
		func(values []string) { redacted = append(redacted, values...) },
		nil,
	)

	watcher.Start(context.Background())
	if err := os.WriteFile(path, []byte(`{"access":"sk-ant-oat01-refreshed-access-token"}`), 0o600); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	select {
	case <-store.stored:
	case <-time.After(5 * time.Second):
		t.Fatal("the refreshed credential was never stored")
	}
	watcher.Stop()

	calls := store.snapshot()
	if len(calls) == 0 || calls[0][0] != "OPENCODE_AUTH" || calls[0][1] != `{"access":"sk-ant-oat01-refreshed-access-token"}` {
		t.Fatalf("store calls = %#v", calls)
	}
	// Redacted before it is stored, so nothing reporting the rotation can carry
	// the new value out.
	found := false
	for _, value := range redacted {
		if value == "sk-ant-oat01-refreshed-access-token" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refreshed credential was not registered for redaction: %#v", redacted)
	}
}

// A rotation that cannot be persisted is a warning, not a failed execution: the
// value the harness is using still works for the run it is in.
func TestARotationThatCannotBeStoredDoesNotFailTheExecution(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, []byte("original-credential"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	store := &recordingStore{err: errors.New("no credential of this name to rotate")}
	watcher := newRotationWatcher(
		[]credentialFile{{Name: "OPENCODE_AUTH", Path: path}}, time.Hour, store.store, nil, nil,
	)
	watcher.Start(context.Background())

	if err := os.WriteFile(path, []byte("refreshed-credential"), 0o600); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	watcher.Stop()

	if calls := store.snapshot(); len(calls) != 1 {
		t.Fatalf("the final pass did not attempt the store: %#v", calls)
	}
}

// Stopping takes one last look: an execution that ended on its deadline still
// holds a valid lease, and the token refreshed just before the end is exactly
// the one worth keeping.
func TestStoppingPersistsARotationTheLastTickMissed(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "auth.json")
	if err := os.WriteFile(path, []byte("original-credential"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	store := &recordingStore{}
	watcher := newRotationWatcher(
		[]credentialFile{{Name: "OPENCODE_AUTH", Path: path}}, time.Hour, store.store, nil, nil,
	)
	watcher.Start(context.Background())
	if err := os.WriteFile(path, []byte("refreshed-credential"), 0o600); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	watcher.Stop()

	calls := store.snapshot()
	if len(calls) != 1 || calls[0][1] != "refreshed-credential" {
		t.Fatalf("store calls = %#v", calls)
	}
}

// End to end through Execute: a provider key declared by the packet reaches the
// agent process, is registered for redaction before it can, and a
// file-delivered one arrives as a path under the agent's own HOME.
func TestExecuteDeliversAProviderCredentialToTheAgent(t *testing.T) {
	manager := &workspaceManager{workspace: credentialWorkspace(t)}
	agent := &backend{result: agents.Result{Status: "completed"}}
	lease := validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{
		{Name: "OPENROUTER_API_KEY", SecretRef: "agent:OPENROUTER_API_KEY"},
		{Name: "OPENCODE_AUTH", SecretRef: "agent:OPENCODE_AUTH", Path: ".local/share/opencode/auth.json"},
	}
	var redacted []string
	// Read while the agent is running: the file is discarded on the way out, so
	// asserting on it afterwards would test the cleanup, not the delivery.
	var seenByHarness string
	agent.onExecute = func(request agents.Request) {
		contents, err := os.ReadFile(request.Environment["OPENCODE_AUTH"])
		if err != nil {
			t.Errorf("the harness could not read its credentials file: %v", err)
			return
		}
		seenByHarness = string(contents)
	}
	dispatcher := Dispatcher{
		Workspaces: manager,
		Backend:    agent,
		Environment: environmentResolver{values: map[string]string{
			"OPENROUTER_API_KEY": "or-v1-9f2c1d4e8a7b6c5d",
			"OPENCODE_AUTH":      `{"access":"sk-ant-oat01-longaccesstoken"}`,
		}},
		AllowedEnvironment: []string{"OPENROUTER_API_KEY", "OPENCODE_AUTH"},
		RedactSecrets:      func(_ string, values []string) { redacted = append(redacted, values...) },
	}

	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got := agent.request.Environment["OPENROUTER_API_KEY"]; got != "or-v1-9f2c1d4e8a7b6c5d" {
		t.Fatalf("agent OPENROUTER_API_KEY = %q", got)
	}
	home := agent.request.Environment["HOME"]
	if home == "" || home == agent.request.Workspace {
		t.Fatalf("agent HOME = %q, want a directory of its own", home)
	}
	authPath := agent.request.Environment["OPENCODE_AUTH"]
	if authPath != filepath.Join(home, ".local", "share", "opencode", "auth.json") {
		t.Fatalf("agent OPENCODE_AUTH = %q, want a path under HOME", authPath)
	}
	if seenByHarness != `{"access":"sk-ant-oat01-longaccesstoken"}` {
		t.Fatalf("credentials file the harness read = %q", seenByHarness)
	}
	// And it is gone once the execution is over, even though this workspace
	// could have been retained.
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("the delivered credential outlived the execution: %v", err)
	}
	for _, want := range []string{"or-v1-9f2c1d4e8a7b6c5d", "sk-ant-oat01-longaccesstoken"} {
		found := false
		for _, value := range redacted {
			if value == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q was handed to the agent without being registered for redaction: %#v", want, redacted)
		}
	}
}

// 49cad05 fixed this class of bug for LOOP_SECRET_KEY: a missing credential has
// to say what to configure, or an operator is left with an agent failing
// against a paid model for no stated reason.
func TestAnUnallowedProviderCredentialNamesTheSettingToChange(t *testing.T) {
	manager := &workspaceManager{workspace: credentialWorkspace(t)}
	agent := &backend{}
	lease := validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{
		{Name: "OPENROUTER_API_KEY", SecretRef: "agent:OPENROUTER_API_KEY"},
	}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            agent,
		Environment:        environmentResolver{values: map[string]string{"OPENROUTER_API_KEY": "or-v1-key"}},
		AllowedEnvironment: []string{"GITHUB_TOKEN"},
	}

	_, err := dispatcher.Execute(context.Background(), lease)

	if err == nil {
		t.Fatal("Execute() accepted a reference outside the allow-list")
	}
	for _, want := range []string{"OPENROUTER_API_KEY", "LOOP_RUNNER_ALLOWED_ENVIRONMENT", "GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Execute() error = %v, want it to mention %q", err, want)
		}
	}
	if agent.request.ExecutionID != "" {
		t.Fatal("the agent ran without the credential it declared")
	}
}

// The value must not be recoverable from the packet the runner writes into the
// workspace for the agent to read.
func TestTheTaskPacketArtifactCarriesNoCredentialValue(t *testing.T) {
	workspace := credentialWorkspace(t)
	manager := &workspaceManager{workspace: workspace}
	lease := validLease()
	lease.Packet.EnvironmentRefs = []taskpacket.EnvironmentRef{
		{Name: "OPENROUTER_API_KEY", SecretRef: "agent:OPENROUTER_API_KEY"},
	}
	dispatcher := Dispatcher{
		Workspaces:         manager,
		Backend:            &backend{result: agents.Result{Status: "completed"}},
		Environment:        environmentResolver{values: map[string]string{"OPENROUTER_API_KEY": "or-v1-9f2c1d4e8a7b6c5d"}},
		AllowedEnvironment: []string{"OPENROUTER_API_KEY"},
	}
	if _, err := dispatcher.Execute(context.Background(), lease); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	artifact, err := os.ReadFile(filepath.Join(workspace.Loop, "task-packet.json"))
	if err != nil {
		t.Fatalf("read task packet artifact: %v", err)
	}
	if strings.Contains(string(artifact), "or-v1-9f2c1d4e8a7b6c5d") {
		t.Fatalf("the task packet artifact carries a credential value: %s", artifact)
	}
	if !strings.Contains(string(artifact), "OPENROUTER_API_KEY") {
		t.Fatalf("the task packet artifact lost its reference: %s", artifact)
	}
}
