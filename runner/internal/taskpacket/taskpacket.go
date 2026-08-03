package taskpacket

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const ProtocolVersion = "1.0"

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

type Role string

const (
	RolePlanner   Role = "planner"
	RoleDeveloper Role = "developer"
	RolePipeline  Role = "pipeline"
	RoleReviewer  Role = "reviewer"
	RoleRepairer  Role = "repairer"
)

type Packet struct {
	ProtocolVersion    string            `json:"protocolVersion"`
	JobID              string            `json:"jobId"`
	ExecutionID        string            `json:"executionId"`
	Role               Role              `json:"role"`
	Objective          string            `json:"objective"`
	Issue              Issue             `json:"issue"`
	Repository         Repository        `json:"repository"`
	PromptPath         string            `json:"promptPath"`
	ExpectedOutput     string            `json:"expectedOutput"`
	TimeoutSeconds     int               `json:"timeoutSeconds"`
	EnvironmentRefs    []EnvironmentRef  `json:"environmentRefs"`
	ExecutionImage     string            `json:"executionImage"`
	Constraints        Constraints       `json:"constraints"`
	Pipeline           []PipelineCommand `json:"pipeline"`
	AcceptanceCriteria []string          `json:"acceptanceCriteria"`
	Plan               []string          `json:"plan"`
	PreviousFailures   []string          `json:"previousFailures"`
	CurrentCommit      string            `json:"currentCommit"`
	DiffSummary        string            `json:"diffSummary"`
	FailedChecks       []string          `json:"failedChecks"`
	ReviewFindings     []string          `json:"reviewFindings"`
}

type Issue struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
	Body       string `json:"body"`
}

type Repository struct {
	ProjectID     string `json:"projectId"`
	Mode          string `json:"mode"`
	URL           string `json:"url"`
	LocalPath     string `json:"localPath"`
	DefaultBranch string `json:"defaultBranch"`
	Branch        string `json:"branch"`
}

type EnvironmentRef struct {
	Name      string `json:"name"`
	SecretRef string `json:"secretRef"`
	// Path, when set, means the resolved value has to be written to a file
	// rather than exported as a variable, at this location *relative to the
	// home directory the runner builds for the execution*. The variable named
	// by Name then carries that path instead of the value.
	//
	// It exists because the runner overrides HOME to a throwaway directory, so
	// a harness looking for ~/.config/... finds nothing that an image baked in.
	// A path is a destination, not a secret: it is safe in the packet.
	Path string `json:"path,omitempty"`
}

type Constraints struct {
	MayModifyFiles bool `json:"mayModifyFiles"`
	MayPush        bool `json:"mayPush"`
	MayMerge       bool `json:"mayMerge"`
}

type PipelineCommand struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

func Parse(contents []byte) (Packet, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var packet Packet
	if err := decoder.Decode(&packet); err != nil {
		return Packet{}, fmt.Errorf("decode task packet: %w", err)
	}
	if decoder.More() {
		return Packet{}, errors.New("task packet contains multiple values")
	}
	if err := packet.Validate(); err != nil {
		return Packet{}, err
	}
	return packet, nil
}

func (packet Packet) Validate() error {
	if packet.ProtocolVersion != ProtocolVersion {
		return errors.New("task packet protocolVersion must be 1.0")
	}
	if !safeIdentifier(packet.JobID) || !safeIdentifier(packet.ExecutionID) {
		return errors.New("task packet job ID and execution ID are required")
	}
	if !safeText(packet.Objective, 32768) || !safeIdentifier(packet.Issue.ExternalID) || !safeText(packet.Issue.Title, 1024) || !safeProse(packet.Issue.Body, 65536) {
		return errors.New("task packet issue or objective is invalid")
	}
	if err := packet.Repository.validate(); err != nil {
		return err
	}
	if !taskPath(packet.PromptPath) || packet.ExpectedOutput != ".loop/result.json" {
		return errors.New("task packet artifact paths are invalid")
	}
	// A packet must always carry a real deadline. This used to accept 0 (#247,
	// reverted by #276) to accommodate a hardcoded orchestrator value -- but a
	// zero timeout meant no deadline at all, so a wedged agent process held its
	// project lock indefinitely: the lease sweep only reclaims a job whose
	// runner has stopped, not one whose agent never returns. The orchestrator
	// now always sends a positive value, so this guard is exercised on every
	// real packet, not just relaxed around one.
	if packet.TimeoutSeconds < 1 || packet.TimeoutSeconds > 86400 {
		return errors.New("task packet timeout is invalid")
	}
	if err := validateEnvironmentRefs(packet.EnvironmentRefs); err != nil {
		return err
	}
	if !safeExecutionImage(packet.ExecutionImage) {
		return errors.New("task packet execution image is invalid")
	}
	if err := validatePipeline(packet.Pipeline); err != nil {
		return err
	}
	if err := validateContext(packet); err != nil {
		return err
	}
	if packet.Constraints.MayMerge {
		return errors.New("task packet must not permit merge")
	}
	switch packet.Role {
	case RolePlanner, RolePipeline, RoleReviewer:
		if packet.Constraints.MayModifyFiles || packet.Constraints.MayPush {
			return errors.New("task packet role cannot modify or push")
		}
	case RoleDeveloper, RoleRepairer:
	default:
		return errors.New("task packet role is invalid")
	}
	return nil
}

func (repository Repository) validate() error {
	if !safeIdentifier(repository.ProjectID) || !safeBranch(repository.DefaultBranch) || (repository.Branch != "" && !safeBranch(repository.Branch)) {
		return errors.New("task packet repository identifiers are invalid")
	}
	switch repository.Mode {
	case "managed_clone":
		if !safeRepositoryURL(repository.URL) || repository.LocalPath != "" {
			return errors.New("managed clone repository is invalid")
		}
	case "existing_path":
		if !safeAbsolutePath(repository.LocalPath) || repository.URL != "" {
			return errors.New("existing path repository is invalid")
		}
	default:
		return errors.New("task packet repository mode is invalid")
	}
	return nil
}

func validatePipeline(commands []PipelineCommand) error {
	if len(commands) > 32 {
		return errors.New("task packet has too many pipeline commands")
	}
	for _, command := range commands {
		if !safeText(command.Command, 4096) || command.TimeoutSeconds < 1 || command.TimeoutSeconds > 3600 {
			return errors.New("task packet pipeline command is invalid")
		}
	}
	return nil
}

func validateContext(packet Packet) error {
	for _, values := range [][]string{
		packet.AcceptanceCriteria,
		packet.Plan,
		packet.PreviousFailures,
		packet.FailedChecks,
		packet.ReviewFindings,
	} {
		if len(values) > 64 {
			return errors.New("task packet context has too many entries")
		}
		for _, value := range values {
			if value == "" || !safeProse(value, 8192) {
				return errors.New("task packet context is invalid")
			}
		}
	}
	if !safeTextAllowEmpty(packet.CurrentCommit, 1024) || !safeProse(packet.DiffSummary, 16384) {
		return errors.New("task packet context is invalid")
	}
	return nil
}

func validateEnvironmentRefs(references []EnvironmentRef) error {
	if len(references) > 64 {
		return errors.New("task packet has too many environment references")
	}
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if !environmentName.MatchString(reference.Name) || !safeText(reference.SecretRef, 512) {
			return errors.New("task packet environment reference is invalid")
		}
		if reference.Path != "" && !credentialFilePath(reference.Path) {
			return errors.New("task packet credential file path is invalid")
		}
		if _, exists := seen[reference.Name]; exists {
			return errors.New("task packet environment references must be unique")
		}
		seen[reference.Name] = struct{}{}
	}
	return nil
}

// credentialFilePath accepts only a destination inside the execution's own home
// directory. Absolute paths and parent traversal are refused here rather than
// where the file is written, so a packet that could place credential material
// outside the directory the runner owns and discards never gets that far.
func credentialFilePath(path string) bool {
	if !safeText(path, 256) || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if strings.HasPrefix(path, "~") || strings.ContainsAny(path, "\\") {
		return false
	}
	for _, segment := range strings.Split(path, string(filepath.Separator)) {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func taskPath(path string) bool {
	if !safeText(path, 1024) || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	return strings.HasPrefix(path, ".loop"+string(filepath.Separator))
}

func safeAbsolutePath(path string) bool {
	return safeText(path, 4096) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func safeRepositoryURL(url string) bool {
	return safeText(url, 4096) && !strings.HasPrefix(url, "-")
}

func safeBranch(branch string) bool {
	return safeText(branch, 255) && !strings.HasPrefix(branch, "-") && !strings.Contains(branch, "..") && !strings.Contains(branch, "//") && !strings.HasSuffix(branch, "/")
}

func safeExecutionImage(image string) bool {
	return len(image) <= 512 && strings.TrimSpace(image) == image && !strings.ContainsAny(image, " \t\r\n\x00")
}

func safeIdentifier(value string) bool {
	return safeText(value, 128) && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, `/\\`)
}

func safeText(value string, maximum int) bool {
	return value != "" && safeTextAllowEmpty(value, maximum)
}

// safeTextAllowEmpty is for single-line values: identifiers, paths, branches,
// URLs, a title. It rejects every control character, which includes newlines,
// and requires the value to be its own TrimSpace.
func safeTextAllowEmpty(value string, maximum int) bool {
	return len(value) <= maximum && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

// safeProse is for the free-form text a human wrote: an issue body, a plan
// step, a review finding, a diff summary. Applying safeTextAllowEmpty to these
// rejected every packet whose issue body had a second line -- which is nearly
// every real issue -- as "task packet issue or objective is invalid", so no
// execution could ever start.
//
// Line structure is the content here, not a smuggling risk: the value is
// written into a prompt file, never into a command line or a shell. What is
// still rejected is the rest of the control range -- NUL, escape, the C1 block
// -- which has no place in prose and is what terminal-escape injection uses.
func safeProse(value string, maximum int) bool {
	if len(value) > maximum {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	}) < 0
}
