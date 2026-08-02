package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

// How often a file-delivered credential is checked for a value the harness
// refreshed. A subscription access token can expire inside one execution -- a
// developer packet is budgeted at an hour -- so the rotated value has to be
// persisted while the run is still going, not only once it ends.
const defaultRotationInterval = 30 * time.Second

// Strings shorter than this are not treated as rotatable credential material.
// A file that briefly holds "{}" or "null" while a harness rewrites it is not a
// credential, and storing it would overwrite the working one with rubbish.
const minimumRotatedCredential = 8

// executionHome is the home directory the agent and pipeline processes are
// given, and the only `~` they ever see: MinimalEnvironment overrides HOME, so
// nothing an image baked into /home/loop is found at run time.
//
// A sibling of the checkout rather than the checkout itself. HOME used to *be*
// the repository working tree, which meant every cache, config file and
// credential a tool writes to ~ landed in the tree the agent was about to
// commit. A credential file there would be one `git add -A` away from the code
// host.
func executionHome(workspace repository.Workspace) string {
	return filepath.Join(workspace.Root, "home")
}

// credentialFile is a resolved credential the harness reads from disk.
type credentialFile struct {
	Name string
	Path string
}

// prepareExecutionEnvironment builds what the agent and pipeline run with:
// the resolved credentials, a home directory of their own, and any credential
// the packet asked to be delivered as a file written into that home.
//
// The repository operations keep the raw environment -- git wants GITHUB_TOKEN
// as a value -- so this deliberately returns a copy rather than mutating it.
func prepareExecutionEnvironment(
	workspace repository.Workspace,
	references []taskpacket.EnvironmentRef,
	environment map[string]string,
) (map[string]string, []credentialFile, error) {
	home := executionHome(workspace)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create agent home directory %s: %w", home, err)
	}
	execution := make(map[string]string, len(environment)+1)
	for name, value := range environment {
		execution[name] = value
	}
	execution["HOME"] = home
	var files []credentialFile
	for _, reference := range references {
		if reference.Path == "" {
			continue
		}
		value, ok := environment[reference.Name]
		if !ok || value == "" {
			continue
		}
		path, err := credentialFilePath(home, reference.Path)
		if err != nil {
			return nil, files, err
		}
		if err := writeCredentialFile(path, value); err != nil {
			return nil, files, err
		}
		// The variable carries the path, never the value. A credential is
		// delivered as a file precisely because the harness reads it from disk;
		// exporting the contents as well would hand them to every child process
		// for nothing, and put them one `env` away from the log stream.
		execution[reference.Name] = path
		files = append(files, credentialFile{Name: reference.Name, Path: path})
	}
	return execution, files, nil
}

// credentialFilePath resolves a packet-declared destination inside the home
// directory, refusing anything that escapes it. The packet is validated on
// arrival too; this is the check that runs against the resolved path, so a
// symlink or a traversal that survived parsing still cannot place credential
// material outside the directory the runner owns and discards.
func credentialFilePath(home, declared string) (string, error) {
	// Refused rather than rebased. filepath.Join("/home/x", "/etc/passwd") is
	// "/home/x/etc/passwd", so an absolute destination would silently become a
	// *different* file that passes every containment check below.
	if declared == "" || filepath.IsAbs(declared) || strings.HasPrefix(declared, "~") {
		return "", fmt.Errorf("credential file path %q must be relative to the agent home directory", declared)
	}
	root, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve agent home directory: %w", err)
	}
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(declared)))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || relative == "." ||
		(len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("credential file path %q is outside the agent home directory", declared)
	}
	return path, nil
}

func writeCredentialFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credential directory %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create credential file %s: %w", path, err)
	}
	defer file.Close()
	if _, err := file.WriteString(value); err != nil {
		return fmt.Errorf("write credential file %s: %w", path, err)
	}
	// An existing file keeps its old mode through O_CREATE, so the private mode
	// is asserted rather than requested.
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure credential file %s: %w", path, err)
	}
	return nil
}

// discardCredentialFiles removes what prepareExecutionEnvironment wrote.
//
// Called on every path out of an execution. It matters most for a *retained*
// workspace: retention keeps a failed run's directory for forensics, and
// without this the credential the run was given would be kept along with it.
func discardCredentialFiles(files []credentialFile) error {
	var failures []error
	for _, file := range files {
		if err := os.Remove(file.Path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// secretLiterals is everything about a resolved value that must never appear in
// a log: the value itself, and -- when it is a credentials document -- each of
// the strings inside it.
//
// A subscription file is JSON holding an access token and a refresh token. The
// document as a whole is never echoed, but a harness reporting "using token
// sk-ant-..." echoes one of its fields, and redacting only the whole document
// would let that through.
func secretLiterals(value string) []string {
	literals := []string{value}
	var document any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return literals
	}
	return append(literals, jsonStringLeaves(document, nil)...)
}

func jsonStringLeaves(value any, into []string) []string {
	switch current := value.(type) {
	case map[string]any:
		for _, item := range current {
			into = jsonStringLeaves(item, into)
		}
	case []any:
		for _, item := range current {
			into = jsonStringLeaves(item, into)
		}
	case string:
		// Short strings in a credentials document are field markers ("Bearer",
		// "oauth", a provider name), not secrets. Registering those would
		// redact ordinary words out of every log line.
		if len(current) >= 16 {
			into = append(into, current)
		}
	}
	return into
}

// rotationWatcher persists credential files the harness rewrites while it runs.
//
// The runner does no OAuth of its own: the harness refreshes its own token and
// writes the new one where it found the old one. All this does is notice and
// make it durable, which is the half that decides whether the *next* execution
// starts from a live token or repeats the authorization dance.
type rotationWatcher struct {
	store    func(ctx context.Context, name, value string) error
	redact   func(values []string)
	files    []credentialFile
	interval time.Duration
	logger   *slog.Logger

	mu     sync.Mutex
	digest map[string]string
	done   chan struct{}
	stop   chan struct{}
	once   sync.Once
}

func newRotationWatcher(
	files []credentialFile,
	interval time.Duration,
	store func(ctx context.Context, name, value string) error,
	redact func(values []string),
	logger *slog.Logger,
) *rotationWatcher {
	if interval <= 0 {
		interval = defaultRotationInterval
	}
	watcher := &rotationWatcher{
		store:    store,
		redact:   redact,
		files:    files,
		interval: interval,
		logger:   logger,
		digest:   make(map[string]string, len(files)),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
	// Whatever was delivered is the baseline, so the value the runner itself
	// just wrote is never mistaken for a rotation and written straight back.
	for _, file := range files {
		if contents, err := os.ReadFile(file.Path); err == nil {
			watcher.digest[file.Path] = digestOf(contents)
		}
	}
	return watcher
}

// Start begins polling. A watcher with nothing to watch, or no way to store
// what it finds, does nothing at all rather than spinning a goroutine.
func (w *rotationWatcher) Start(ctx context.Context) {
	if w == nil || len(w.files) == 0 || w.store == nil {
		close(w.done)
		return
	}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.sync(ctx)
			}
		}
	}()
}

// Stop ends the polling and takes one last look.
//
// The final pass runs on its own context: an execution that ended because its
// deadline passed still holds a valid lease, and the token the harness
// refreshed thirty seconds before the end is exactly the one worth keeping. The
// lease fence in the control plane, not this timeout, decides whether the write
// is allowed.
func (w *rotationWatcher) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() { close(w.stop) })
	<-w.done
	if len(w.files) == 0 || w.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w.sync(ctx)
}

func (w *rotationWatcher) sync(ctx context.Context) {
	for _, file := range w.files {
		value, changed := w.changed(file.Path)
		if !changed {
			continue
		}
		if len(value) < minimumRotatedCredential {
			continue
		}
		// Redacted before it is stored, and therefore before anything can
		// report the rotation. The value the agent refreshed to is as sensitive
		// as the one it replaced.
		if w.redact != nil {
			w.redact(secretLiterals(value))
		}
		if err := w.store(ctx, file.Name, value); err != nil {
			// Never fatal to the execution: the value the harness is using
			// still works for this run. It is logged because the consequence --
			// the next execution re-authorizing, or failing -- shows up far
			// from here.
			w.log().Warn("could not persist a rotated credential", "name", file.Name, "error", err)
			continue
		}
		w.log().Info("persisted a credential the agent rotated", "name", file.Name)
	}
}

// changed reports the file's contents when they differ from what was last seen.
func (w *rotationWatcher) changed(path string) (string, bool) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := digestOf(contents)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.digest[path] == sum {
		return "", false
	}
	w.digest[path] = sum
	return string(contents), true
}

func digestOf(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

func (w *rotationWatcher) log() *slog.Logger {
	if w.logger != nil {
		return w.logger
	}
	return slog.Default()
}
