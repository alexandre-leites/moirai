package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// FieldKind is how a Field's value should be rendered and validated. It is
// the whole reason the console never has to know a provider by name: every
// kind maps to exactly one form control (text input, number input, checkbox,
// select, tag list, or a masked secret input with a "configured/not
// configured" indicator instead of a value).
type FieldKind int

const (
	FieldText FieldKind = iota
	FieldNumber
	FieldBool
	FieldEnum
	FieldStringList
	FieldSecret
)

// String renders a FieldKind the way it crosses the wire (discovery RPC
// responses and error messages): a stable lowercase token, not the Go
// identifier, so a future field kind added here does not silently change an
// already-shipped console's parsing.
func (kind FieldKind) String() string {
	switch kind {
	case FieldText:
		return "text"
	case FieldNumber:
		return "number"
	case FieldBool:
		return "bool"
	case FieldEnum:
		return "enum"
	case FieldStringList:
		return "string_list"
	case FieldSecret:
		return "secret"
	default:
		return "text"
	}
}

// Field is one configuration input a TaskSourceType needs. This is the
// declaration the issue asks for: everything a generic form renderer (or the
// server's own validator) needs to know about one key in a task source's
// configuration, without either of them knowing the provider by name.
//
// Deliberately no separate `Secret bool` alongside Kind, unlike the issue's
// literal sketch: Kind == FieldSecret already says everything "is this
// secret" would say, and a bool that could disagree with Kind is a footgun
// this design has no reason to accept. IsSecret() below is the one place
// that fact is asked.
type Field struct {
	Key      string
	Label    string
	Help     string
	Kind     FieldKind
	Required bool
	// Default is the value a create form should pre-fill and an absent
	// optional field resolves to when validating. Never itself a secret --
	// FieldSecret fields must leave this nil, enforced by validateFields at
	// registration.
	Default any
	// Options enumerates the valid values for a FieldEnum field. Ignored for
	// every other kind.
	Options []string
	// Pattern, when non-empty, is a regexp a FieldText value must match.
	// Ignored for every other kind.
	Pattern string
	// CredentialKind, for a Secret field only, is the app.project_credentials
	// "kind" this field's value is stored under (source-scoped via
	// task_source_id, see migration 026). Empty means the generated default
	// ("source_field:<key>", see tasksources_rpc.go's credentialKind).
	// GitHub's "token" field overrides this to "github_token": that kind,
	// scoped by task_source_id, is exactly what resolveGitHubToken
	// (github.go) already looks up for every TaskSource call, and predates
	// this issue -- giving it its own generated kind instead would silently
	// orphan every secret this RPC writes from the adapter that is supposed
	// to read it.
	CredentialKind string
}

// IsSecret reports whether this field must never be stored in
// project_task_sources.configuration or returned by any RPC (see
// tasksources_rpc.go's secret routing).
func (field Field) IsSecret() bool {
	return field.Kind == FieldSecret
}

// TaskSourceType is a provider's own declaration of what it needs: an id
// used as app.project_task_sources.provider, an operator-facing name, and
// the fields above. Deliberately narrower than the issue's own sketch, which
// also proposed a `New(config, secrets) (TaskSource, error)` constructor:
// this codebase already resolves a source's ref and credentials per call
// (ListTasks(ctx, projectID, taskSourceID, ref), resolveGitHubToken keyed on
// taskSourceID -- see tasksource.go/github.go) rather than constructing a
// fresh TaskSource instance per configured source, so a descriptor-driven
// constructor would duplicate a seam that already exists and already covers
// per-source secrets and per-source configuration. TaskSourceType here is
// deliberately just the descriptor: what discovery and validation need.
type TaskSourceType interface {
	ID() string
	DisplayName() string
	Fields() []Field
}

type taskSourceType struct {
	id, displayName string
	fields          []Field
}

func (t taskSourceType) ID() string          { return t.id }
func (t taskSourceType) DisplayName() string { return t.displayName }
func (t taskSourceType) Fields() []Field     { return t.fields }

var fieldKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// newTaskSourceType validates a descriptor at registration time (a
// programmer error in a hardcoded descriptor should fail a test, not a
// runtime request) and returns it wrapped as a TaskSourceType.
func newTaskSourceType(id, displayName string, fields []Field) taskSourceType {
	if id == "" || displayName == "" {
		panic(fmt.Sprintf("task source type %q: id and display name are required", id))
	}
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		if !fieldKeyPattern.MatchString(field.Key) {
			panic(fmt.Sprintf("task source type %q: field key %q is invalid", id, field.Key))
		}
		if seen[field.Key] {
			panic(fmt.Sprintf("task source type %q: duplicate field key %q", id, field.Key))
		}
		seen[field.Key] = true
		if field.Label == "" {
			panic(fmt.Sprintf("task source type %q: field %q has no label", id, field.Key))
		}
		if field.IsSecret() && field.Default != nil {
			panic(fmt.Sprintf("task source type %q: secret field %q must not declare a default", id, field.Key))
		}
		if !field.IsSecret() && field.CredentialKind != "" {
			panic(fmt.Sprintf("task source type %q: field %q declares a CredentialKind but is not Secret", id, field.Key))
		}
		if field.Kind == FieldEnum && len(field.Options) == 0 {
			panic(fmt.Sprintf("task source type %q: enum field %q declares no options", id, field.Key))
		}
	}
	return taskSourceType{id: id, displayName: displayName, fields: fields}
}

// githubTaskSourceType is the descriptor for the "github" provider
// (github.go's githubCLI). Only ref and token are declared: label_filter is
// deliberately not, because issuePriority's agent:ready/agent:blocked/
// agent-priority:N convention is not (yet) read from configuration -- adding
// a field the server would accept but the adapter ignores would be exactly
// the "descriptor says one thing, behavior says another" bug this issue
// exists to prevent. See the PR description for the tracked follow-up.
var githubTaskSourceType = newTaskSourceType("github", "GitHub", []Field{
	{
		Key:      "ref",
		Label:    "Repository",
		Help:     "owner/name, or a github.com URL",
		Kind:     FieldText,
		Required: true,
		Pattern:  `^(https://github\.com/|git@github\.com:)?[\w.-]+/[\w.-]+(\.git)?$`,
	},
	{
		Key:            "token",
		Label:          "Personal access token",
		Help:           "Falls back to the orchestrator's global GitHub token when not set.",
		Kind:           FieldSecret,
		CredentialKind: "github_token",
	},
})

// localFileTaskSourceType is the descriptor for the "local_file" provider
// (localfile.go's LocalFileTaskSource), the seam's proof-of-concept second
// adapter. Only ref (a directory path) is declared: the adapter reads every
// "*.json" file in it unconditionally, so a configurable glob is not
// something the adapter can honor yet.
var localFileTaskSourceType = newTaskSourceType("local_file", "Local file directory", []Field{
	{
		Key:      "ref",
		Label:    "Directory path",
		Help:     "Directory containing one *.json file per task.",
		Kind:     FieldText,
		Required: true,
	},
})

// taskSourceTypeRegistry is every TaskSourceType this orchestrator can
// validate and describe, keyed by provider id. resolveTaskSource
// (adapters.go) is the parallel registry that constructs the runtime
// adapter; this one exists purely for discovery and validation and is kept
// deliberately separate so #294's descriptor work never has to touch how a
// sync actually runs.
var taskSourceTypeRegistry = map[string]TaskSourceType{
	githubTaskSourceType.ID():    githubTaskSourceType,
	localFileTaskSourceType.ID(): localFileTaskSourceType,
}

// taskSourceTypes returns every registered TaskSourceType, sorted by id, for
// the discovery RPC (ListTaskSourceTypes).
func taskSourceTypes() []TaskSourceType {
	types := make([]TaskSourceType, 0, len(taskSourceTypeRegistry))
	for _, t := range taskSourceTypeRegistry {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i].ID() < types[j].ID() })
	return types
}

// lookupTaskSourceType returns the descriptor for provider, or false when no
// adapter declares one -- including "" and "github" being distinct from the
// legacy runtime fallback resolveTaskSource applies (adapters.go): the
// runtime is permissive for data migration 026 already put in the database,
// but a *new* write always names its provider explicitly and must match a
// real descriptor.
func lookupTaskSourceType(provider string) (TaskSourceType, bool) {
	t, ok := taskSourceTypeRegistry[provider]
	return t, ok
}

// validationError is a single field's validation failure, joined together by
// validateConfiguration into one message that names every problem at once
// rather than making a caller fix and resubmit one field at a time.
type validationError struct {
	field  string
	reason string
}

func (e validationError) Error() string { return fmt.Sprintf("%s: %s", e.field, e.reason) }

// validateConfiguration checks config (the decoded non-secret portion of a
// task source's configuration JSONB) and secretKeys (which secret field keys
// have a value provided or already configured) against fields, and returns
// every problem found. Both the create and update RPC handlers call this
// with the same descriptor the discovery RPC hands the console, which is
// what keeps the two unable to disagree (see the issue body's "Validation
// belongs on the server").
func validateConfiguration(fields []Field, config map[string]any, secretKeys map[string]bool) error {
	var problems []error
	seen := make(map[string]bool, len(config))
	for key := range config {
		seen[key] = true
	}
	for _, field := range fields {
		if field.IsSecret() {
			if field.Required && !secretKeys[field.Key] {
				problems = append(problems, validationError{field.Key, "is required"})
			}
			continue
		}
		value, present := config[field.Key]
		if !present {
			if field.Required {
				problems = append(problems, validationError{field.Key, "is required"})
			}
			continue
		}
		if err := validateFieldValue(field, value); err != nil {
			problems = append(problems, validationError{field.Key, err.Error()})
		}
	}
	// Unknown keys (including a client that tried to smuggle a secret field's
	// value into the non-secret configuration map) are rejected rather than
	// silently dropped or silently stored: a malformed configuration must not
	// reach an adapter, which is exactly what an ignored typo would let
	// through.
	known := make(map[string]bool, len(fields))
	for _, field := range fields {
		known[field.Key] = true
	}
	for key := range seen {
		if !known[key] {
			problems = append(problems, validationError{key, "is not a field this provider accepts"})
		}
	}
	if len(problems) == 0 {
		return nil
	}
	messages := make([]string, len(problems))
	for i, problem := range problems {
		messages[i] = problem.Error()
	}
	return fmt.Errorf("invalid task source configuration: %s", strings.Join(messages, "; "))
}

func validateFieldValue(field Field, value any) error {
	switch field.Kind {
	case FieldText, FieldEnum:
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if strings.TrimSpace(text) == "" && field.Required {
			return fmt.Errorf("is required")
		}
		if field.Kind == FieldEnum {
			for _, option := range field.Options {
				if option == text {
					return nil
				}
			}
			return fmt.Errorf("must be one of %s", strings.Join(field.Options, ", "))
		}
		if field.Pattern != "" {
			matched, err := regexp.MatchString(field.Pattern, text)
			if err != nil {
				return fmt.Errorf("has an invalid validation pattern")
			}
			if !matched {
				return fmt.Errorf("does not match the required format")
			}
		}
		return nil
	case FieldNumber:
		switch value.(type) {
		case float64, int, int64:
			return nil
		default:
			return fmt.Errorf("must be a number")
		}
	case FieldBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("must be a boolean")
		}
		return nil
	case FieldStringList:
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("must be a list of strings")
		}
		for _, entry := range list {
			if _, ok := entry.(string); !ok {
				return fmt.Errorf("must be a list of strings")
			}
		}
		return nil
	default:
		return fmt.Errorf("has an unsupported field kind")
	}
}

// decodeConfiguration parses a task source's non-secret configuration JSON
// object into a generic map for validateConfiguration. An empty string
// (never persisted, but a defensive default) decodes to an empty object
// rather than an error.
func decodeConfiguration(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("configuration is not a JSON object: %w", err)
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	return decoded, nil
}
