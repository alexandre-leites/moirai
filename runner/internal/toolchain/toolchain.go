// Package toolchain reads the declaration an execution environment makes about
// the tools it offers the agent.
//
// An agent that is not told what it has probes for it, and a probe that fails
// costs a whole attempt: a planning node is allowed two, so two runs that ended
// in `python3: not found` block a workflow permanently. The fix is a
// declaration rather than a discovery, and it belongs to the image rather than
// to the control plane -- the image is the only thing that knows what is
// installed in it, and with per-project execution images it is also the only
// thing that knows which environment a job landed in.
//
// The declaration therefore travels with the image, at a conventional path
// (DefaultManifestPath), in the same spirit as /etc/os-release. Any image
// intended to run Moirai jobs can publish one, and the runner renders whatever
// it finds into the agent's prompt.
package toolchain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultManifestPath is where an execution image publishes its declaration.
// It is a convention rather than a setting: the point is that any image can be
// asked the same question at the same place, without the control plane having
// to describe the image it is dispatching to.
const DefaultManifestPath = "/etc/moirai/toolchain.json"

// SchemaVersion is the only manifest schema this runner understands. A manifest
// written to a later schema is rejected rather than partially read, because a
// half-understood declaration is worse than none: the agent would be told the
// list is complete when it is not.
const SchemaVersion = "1.0"

// Bounds on a document that is read from an image the operator chose and then
// spliced into an agent prompt. They keep a hostile or broken manifest from
// becoming an unbounded prompt.
const (
	maxManifestBytes = 64 << 10
	maxTools         = 128
	maxAbsent        = 128
	maxNotes         = 16
	maxTextBytes     = 1024
)

var toolName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$`)

// Manifest is an execution environment's own account of its toolchain.
type Manifest struct {
	SchemaVersion string `json:"schemaVersion"`
	// Image names the environment the declaration describes, so an agent can
	// see which one it landed in rather than inferring it.
	Image   string   `json:"image"`
	Summary string   `json:"summary"`
	Tools   []Tool   `json:"tools"`
	Absent  []Absent `json:"absent"`
	Notes   []string `json:"notes"`
}

// Tool is a program the environment promises the agent it can run.
type Tool struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
}

// Absent is a program the environment promises the agent it will *not* find.
//
// Declaring absences is the half that actually saves the attempt: an agent
// reaches for `python3` or `make` because they are ordinarily there, and only a
// statement that they are not stops it finding out by running one.
type Absent struct {
	Name string `json:"name"`
	Note string `json:"note"`
}

// ErrNoManifest reports an environment that publishes no declaration. It is not
// a defect -- a runner on a bare host, or an execution image that predates the
// convention, simply has nothing to say -- so callers report the agent nothing
// rather than failing the job.
var ErrNoManifest = errors.New("environment publishes no toolchain manifest")

// Load reads and validates the manifest at path.
func Load(path string) (Manifest, error) {
	if path == "" {
		return Manifest{}, errors.New("toolchain manifest path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%w: %s", ErrNoManifest, filepath.Clean(path))
		}
		return Manifest{}, fmt.Errorf("read toolchain manifest: %w", err)
	}
	if info.Size() > maxManifestBytes {
		return Manifest{}, fmt.Errorf("toolchain manifest is larger than %d bytes", maxManifestBytes)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read toolchain manifest: %w", err)
	}
	return Parse(contents)
}

// Parse decodes and validates a manifest document. Unknown fields are rejected:
// a field this runner does not understand is a declaration it would silently
// drop, and the agent is told the declaration is complete.
func Parse(contents []byte) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode toolchain manifest: %w", err)
	}
	if decoder.More() {
		return Manifest{}, errors.New("toolchain manifest contains multiple values")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("toolchain manifest schemaVersion must be %s", SchemaVersion)
	}
	if !safeText(manifest.Image) || !safeText(manifest.Summary) {
		return errors.New("toolchain manifest image and summary are required")
	}
	if len(manifest.Tools) == 0 {
		return errors.New("toolchain manifest must declare at least one tool")
	}
	if len(manifest.Tools) > maxTools || len(manifest.Absent) > maxAbsent || len(manifest.Notes) > maxNotes {
		return errors.New("toolchain manifest declares too many entries")
	}
	// One name, one meaning. A tool listed as both present and absent would put
	// a contradiction in front of the agent, which is worse than saying nothing.
	seen := make(map[string]struct{}, len(manifest.Tools)+len(manifest.Absent))
	for _, tool := range manifest.Tools {
		if !toolName.MatchString(tool.Name) || !safeText(tool.Purpose) {
			return fmt.Errorf("toolchain manifest tool %q is invalid", tool.Name)
		}
		if _, exists := seen[tool.Name]; exists {
			return fmt.Errorf("toolchain manifest declares %q twice", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
	for _, absent := range manifest.Absent {
		if !toolName.MatchString(absent.Name) || !safeText(absent.Note) {
			return fmt.Errorf("toolchain manifest absence %q is invalid", absent.Name)
		}
		if _, exists := seen[absent.Name]; exists {
			return fmt.Errorf("toolchain manifest declares %q twice", absent.Name)
		}
		seen[absent.Name] = struct{}{}
	}
	for _, note := range manifest.Notes {
		if !safeText(note) {
			return errors.New("toolchain manifest note is invalid")
		}
	}
	return nil
}

// Verify checks the declaration against the environment it claims to describe:
// every declared tool must resolve, and every declared absence must not.
//
// Both directions matter. A declared tool that is missing is the failure the
// declaration exists to prevent, told to the agent as a fact. A declared
// absence that is actually present is how the document rots -- someone installs
// `make`, the manifest still says it is absent, and the agent is talked out of
// using a tool it has. Running this at image build time is what keeps the
// declaration honest without anybody having to remember to.
func (manifest Manifest) Verify(exists func(string) bool) error {
	if exists == nil {
		return errors.New("toolchain verification needs a lookup")
	}
	var missing, unexpected []string
	for _, tool := range manifest.Tools {
		if !exists(tool.Name) {
			missing = append(missing, tool.Name)
		}
	}
	for _, absent := range manifest.Absent {
		if exists(absent.Name) {
			unexpected = append(unexpected, absent.Name)
		}
	}
	var failures []string
	if len(missing) > 0 {
		failures = append(failures, fmt.Sprintf("declared but not installed: %s", strings.Join(missing, ", ")))
	}
	if len(unexpected) > 0 {
		failures = append(failures, fmt.Sprintf("declared absent but installed: %s", strings.Join(unexpected, ", ")))
	}
	if len(failures) > 0 {
		return fmt.Errorf("toolchain manifest does not match image %q: %s", manifest.Image, strings.Join(failures, "; "))
	}
	return nil
}

// LookupIn reports whether name resolves to an executable file on the given
// PATH. It takes the PATH explicitly rather than reading the environment so
// that verification can be run against the PATH the *agent* will be given,
// which is not the one the runner process happens to hold.
func LookupIn(path, name string) bool {
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return false
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return true
	}
	return false
}

// Declaration renders the manifest as the prompt section the agent reads.
//
// It is prose rather than the raw JSON because the reader is a language model
// working from a prompt, and because the absences need a reason attached to
// them: "no make" invites an attempt to install one, "no make, read the
// Makefile and run the commands underneath" does not.
func (manifest Manifest) Declaration() string {
	var builder strings.Builder
	builder.WriteString("You are running inside `")
	builder.WriteString(manifest.Image)
	builder.WriteString("`. ")
	builder.WriteString(manifest.Summary)
	builder.WriteString("\n\nAvailable:\n")
	for _, tool := range manifest.Tools {
		fmt.Fprintf(&builder, "- `%s` — %s\n", tool.Name, tool.Purpose)
	}
	if len(manifest.Absent) > 0 {
		builder.WriteString("\nNot installed:\n")
		for _, absent := range manifest.Absent {
			fmt.Fprintf(&builder, "- `%s` — %s\n", absent.Name, absent.Note)
		}
	}
	for _, note := range manifest.Notes {
		builder.WriteString("\n")
		builder.WriteString(note)
		builder.WriteString("\n")
	}
	return builder.String()
}

// safeText accepts the free-form strings a manifest carries. They are written
// into an agent prompt, so line structure is allowed but the rest of the
// control range -- NUL, escape, the C1 block, which is what terminal-escape
// injection uses -- is not.
func safeText(value string) bool {
	if value == "" || len(value) > maxTextBytes || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
	}) < 0
}
