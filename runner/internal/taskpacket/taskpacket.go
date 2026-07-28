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
	ProtocolVersion string            `json:"protocolVersion"`
	JobID           string            `json:"jobId"`
	ExecutionID     string            `json:"executionId"`
	Role            Role              `json:"role"`
	Objective       string            `json:"objective"`
	Issue           Issue             `json:"issue"`
	Repository      Repository        `json:"repository"`
	PromptPath      string            `json:"promptPath"`
	ExpectedOutput  string            `json:"expectedOutput"`
	TimeoutSeconds  int               `json:"timeoutSeconds"`
	EnvironmentRefs []EnvironmentRef  `json:"environmentRefs"`
	Constraints     Constraints       `json:"constraints"`
	Pipeline        []PipelineCommand `json:"pipeline"`
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
	if !safeText(packet.Objective, 32768) || !safeIdentifier(packet.Issue.ExternalID) || !safeText(packet.Issue.Title, 1024) || !safeTextAllowEmpty(packet.Issue.Body, 65536) {
		return errors.New("task packet issue or objective is invalid")
	}
	if err := packet.Repository.validate(); err != nil {
		return err
	}
	if !taskPath(packet.PromptPath) || packet.ExpectedOutput != ".loop/result.json" {
		return errors.New("task packet artifact paths are invalid")
	}
	if packet.TimeoutSeconds < 1 || packet.TimeoutSeconds > 86400 {
		return errors.New("task packet timeout is invalid")
	}
	if err := validateEnvironmentRefs(packet.EnvironmentRefs); err != nil {
		return err
	}
	if err := validatePipeline(packet.Pipeline); err != nil {
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

func validateEnvironmentRefs(references []EnvironmentRef) error {
	if len(references) > 64 {
		return errors.New("task packet has too many environment references")
	}
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if !environmentName.MatchString(reference.Name) || !safeText(reference.SecretRef, 512) {
			return errors.New("task packet environment reference is invalid")
		}
		if _, exists := seen[reference.Name]; exists {
			return errors.New("task packet environment references must be unique")
		}
		seen[reference.Name] = struct{}{}
	}
	return nil
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

func safeIdentifier(value string) bool {
	return safeText(value, 128) && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, `/\\`)
}

func safeText(value string, maximum int) bool {
	return value != "" && safeTextAllowEmpty(value, maximum)
}

func safeTextAllowEmpty(value string, maximum int) bool {
	return len(value) <= maximum && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}
