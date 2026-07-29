package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadUsesDefaultsAndParsesRunnerEnvironment(t *testing.T) {
	registrationTokenFile := filepath.Join(t.TempDir(), "registration-token")
	if err := os.WriteFile(registrationTokenFile, []byte("registration-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"LOOP_ORCHESTRATOR_ENDPOINT":      "control.example:7443",
		"LOOP_RUNNER_DATA_DIR":            "/var/lib/loop",
		"LOOP_RUNNER_NAME":                "worker-a",
		"LOOP_RUNNER_LABELS":              "linux, docker,opencode",
		"LOOP_RUNNER_ALLOWED_ENVIRONMENT": "GITHUB_TOKEN,PROJECT_KEY",

		"LOOP_RUNNER_REGISTRATION_TOKEN_FILE":    registrationTokenFile,
		"LOOP_RUNNER_HEARTBEAT_INTERVAL":         "15s",
		"LOOP_RUNNER_RECONNECT_MIN":              "2s",
		"LOOP_RUNNER_RECONNECT_MAX":              "20s",
		"LOOP_ORCHESTRATOR_TLS":                  "true",
		"LOOP_ORCHESTRATOR_TLS_CA_FILE":          "/etc/loop/ca.pem",
		"LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE": "/etc/loop/client.pem",
		"LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE":  "/etc/loop/client-key.pem",
		"LOOP_ORCHESTRATOR_TLS_SERVER_NAME":      "control.internal",
		"LOOP_RUNNER_MINIMUM_FREE_BYTES":         "2147483648",
		"LOOP_RUNNER_REDACTION_PREFIXES":         "internal_,custom-",
		"LOOP_RUNNER_DOCKER_ENABLED":             "true",
		"LOOP_RUNNER_RETAIN_WORKSPACES":          "succeeded,failed",
		"LOOP_RUNNER_AGENT_BACKEND":              "cli",
		"LOOP_RUNNER_AGENT_BINARY":               "custom-agent",
		"LOOP_RUNNER_AGENT_DOCKER_IMAGE":         "example/agent:1",
		"LOOP_RUNNER_AGENT_ARGUMENTS":            "run,--safe",
		"LOOP_RUNNER_GIT_COMMITTER_NAME":         "Moirai Bot",
		"LOOP_RUNNER_GIT_COMMITTER_EMAIL":        "bot@example.invalid",
		"LOOP_RUNNER_LEASE_DURATION":             "90s",
		"LOOP_RUNNER_LEASE_RENEWAL_LEAD":         "20s",
		"LOOP_RUNNER_EVENT_BUFFER_SIZE":          "256",
		"LOOP_RUNNER_DOCKER_NETWORK":             "runner-net",
		"LOOP_RUNNER_DOCKER_CPU_LIMIT":           "2",
		"LOOP_RUNNER_DOCKER_MEMORY_LIMIT":        "1g",
	}

	config, err := Load(lookup(environment), func() (string, error) { return "hostname", nil })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OrchestratorEndpoint != "control.example:7443" || config.DataDir != "/var/lib/loop" || config.RunnerName != "worker-a" {
		t.Fatalf("Load() configuration = %#v", config)
	}
	if !config.TLS || config.HeartbeatInterval != 15*time.Second || config.ReconnectMin != 2*time.Second || config.ReconnectMax != 20*time.Second || config.MinimumFreeBytes != 2147483648 {
		t.Fatalf("Load() timings or TLS = %#v", config)
	}
	if config.TLSCAFile != "/etc/loop/ca.pem" || config.TLSClientCertFile != "/etc/loop/client.pem" || config.TLSClientKeyFile != "/etc/loop/client-key.pem" || config.TLSServerName != "control.internal" {
		t.Fatalf("Load() TLS configuration = %#v", config)
	}
	if got, want := config.IdentityPath(), "/var/lib/loop/identity/runner.json"; got != want {
		t.Fatalf("IdentityPath() = %q, want %q", got, want)
	}
	if got, want := config.EventOutboxPath(), "/var/lib/loop/outbox/events.json"; got != want {
		t.Fatalf("EventOutboxPath() = %q, want %q", got, want)
	}
	if got, want := len(config.Labels), 3; got != want || config.Labels[1] != "docker" {
		t.Fatalf("Load() labels = %#v", config.Labels)
	}
	if got, want := len(config.AllowedEnvironment), 2; got != want || config.AllowedEnvironment[1] != "PROJECT_KEY" {
		t.Fatalf("Load() allowed environment = %#v", config.AllowedEnvironment)
	}
	if got, want := len(config.RedactionPrefixes), 2; got != want || config.RedactionPrefixes[0] != "internal_" {
		t.Fatalf("Load() redaction prefixes = %#v", config.RedactionPrefixes)
	}
	if !config.DockerEnabled {
		t.Fatal("Load() DockerEnabled = false, want true")
	}
	if got, want := len(config.WorkspaceRetention), 2; got != want || config.WorkspaceRetention[0] != "succeeded" {
		t.Fatalf("Load() workspace retention = %#v", config.WorkspaceRetention)
	}
	if config.AgentBackend != "cli" || config.AgentBinary != "custom-agent" || len(config.AgentArguments) != 2 {
		t.Fatalf("Load() agent backend = %q/%q/%#v", config.AgentBackend, config.AgentBinary, config.AgentArguments)
	}
	if config.RegistrationToken != "registration-token" {
		t.Fatalf("Load() registration token = %q", config.RegistrationToken)
	}
	if config.GitCommitterName != "Moirai Bot" || config.GitCommitterEmail != "bot@example.invalid" || config.LeaseDuration != 90*time.Second || config.LeaseRenewalLead != 20*time.Second || config.EventBufferSize != 256 || config.DockerNetwork != "runner-net" || config.DockerCPULimit != "2" || config.DockerMemoryLimit != "1g" {
		t.Fatalf("Load() runner reliability settings = %#v", config)
	}
}

func TestLoadDefaultsEndpointDataDirectoryAndHostname(t *testing.T) {
	config, err := Load(lookup(nil), func() (string, error) { return "runner-host", nil })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OrchestratorEndpoint != "orchestrator:50051" || config.DataDir != "/data" || config.RunnerName != "runner-host" {
		t.Fatalf("Load() defaults = %#v", config)
	}
	if config.TLS || config.HeartbeatInterval != 10*time.Second || config.ReconnectMin != time.Second || config.ReconnectMax != time.Minute {
		t.Fatalf("Load() timing defaults = %#v", config)
	}
}

// TestLoadRetainsFailedWorkspacesWithinBoundsByDefault pins the default that
// makes iterative repair possible: a failed run's worktree, terminal result,
// and agent logs survive for the next attempt, and they are bounded so that
// keeping them cannot fill the runner's disk.
func TestLoadRetainsFailedWorkspacesWithinBoundsByDefault(t *testing.T) {
	config, err := Load(lookup(nil), func() (string, error) { return "runner-host", nil })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(config.WorkspaceRetention) != 1 || config.WorkspaceRetention[0] != "failed" {
		t.Fatalf("Load() workspace retention = %#v, want failed runs retained by default", config.WorkspaceRetention)
	}
	if config.RetentionMaxAge != 72*time.Hour || config.RetentionMaxCount != 10 {
		t.Fatalf("Load() retention bounds = %v/%d", config.RetentionMaxAge, config.RetentionMaxCount)
	}
	if !config.PushWorkInProgress {
		t.Fatal("Load() PushWorkInProgress = false, want failed work published by default")
	}
}

func TestLoadAcceptsExplicitRetentionOverrides(t *testing.T) {
	config, err := Load(lookup(map[string]string{
		"LOOP_RUNNER_RETAIN_WORKSPACES":        "none",
		"LOOP_RUNNER_RETENTION_MAX_AGE":        "6h",
		"LOOP_RUNNER_RETENTION_MAX_WORKSPACES": "3",
		"LOOP_RUNNER_PUSH_WORK_IN_PROGRESS":    "false",
	}), func() (string, error) { return "runner-host", nil })
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(config.WorkspaceRetention) != 0 {
		t.Fatalf("Load() workspace retention = %#v, want none", config.WorkspaceRetention)
	}
	if config.RetentionMaxAge != 6*time.Hour || config.RetentionMaxCount != 3 || config.PushWorkInProgress {
		t.Fatalf("Load() retention overrides = %#v", config)
	}
}

func TestValidateRejectsUnboundedRetention(t *testing.T) {
	base, err := Load(lookup(nil), func() (string, error) { return "runner-host", nil })
	if err != nil {
		t.Fatal(err)
	}
	unboundedAge := base
	unboundedAge.RetentionMaxAge = 0
	unboundedCount := base
	unboundedCount.RetentionMaxCount = 0
	for name, candidate := range map[string]Config{"no age bound": unboundedAge, "no count bound": unboundedCount} {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("Validate() accepted retention with %s", name)
		}
	}
	disabled := unboundedAge
	disabled.WorkspaceRetention = nil
	if err := disabled.Validate(); err != nil {
		t.Fatalf("Validate() rejected disabled retention: %v", err)
	}
}

func TestLoadRejectsUnreadableRegistrationTokenFile(t *testing.T) {
	if _, err := Load(lookup(map[string]string{"LOOP_RUNNER_REGISTRATION_TOKEN_FILE": "/missing/token"}), func() (string, error) { return "runner", nil }); err == nil {
		t.Fatal("Load() accepted an unreadable registration token file")
	}
}

func TestSecretValuePrefersMountedSecretFileOverPlainVariable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github_token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "mounted secret file", env: map[string]string{"GITHUB_TOKEN_FILE": path, "GITHUB_TOKEN": "plain-token"}, want: "file-token"},
		{name: "plain variable", env: map[string]string{"GITHUB_TOKEN": "plain-token"}, want: "plain-token"},
		{name: "unconfigured", env: map[string]string{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := SecretValue(lookup(test.env), "GITHUB_TOKEN")
			if err != nil || value != test.want {
				t.Fatalf("SecretValue() = %q, %v, want %q", value, err, test.want)
			}
		})
	}
	if _, err := SecretValue(lookup(map[string]string{"GITHUB_TOKEN_FILE": "/missing/github_token"}), "GITHUB_TOKEN"); err == nil {
		t.Fatal("SecretValue() accepted an unreadable secret file")
	}
}

func TestLoadRejectsUnsafeOrInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "missing port", env: map[string]string{"LOOP_ORCHESTRATOR_ENDPOINT": "orchestrator"}},
		{name: "relative data directory", env: map[string]string{"LOOP_RUNNER_DATA_DIR": "data"}},
		{name: "invalid tls", env: map[string]string{"LOOP_ORCHESTRATOR_TLS": "sometimes"}},
		{name: "TLS option without TLS", env: map[string]string{"LOOP_ORCHESTRATOR_TLS_CA_FILE": "/etc/loop/ca.pem"}},
		{name: "relative TLS CA", env: map[string]string{"LOOP_ORCHESTRATOR_TLS": "true", "LOOP_ORCHESTRATOR_TLS_CA_FILE": "ca.pem"}},
		{name: "unpaired TLS client certificate", env: map[string]string{"LOOP_ORCHESTRATOR_TLS": "true", "LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE": "/etc/loop/client.pem"}},
		{name: "unsafe TLS server name", env: map[string]string{"LOOP_ORCHESTRATOR_TLS": "true", "LOOP_ORCHESTRATOR_TLS_SERVER_NAME": "control\ninternal"}},
		{name: "invalid duration", env: map[string]string{"LOOP_RUNNER_HEARTBEAT_INTERVAL": "0s"}},
		{name: "reconnect ordering", env: map[string]string{"LOOP_RUNNER_RECONNECT_MIN": "10s", "LOOP_RUNNER_RECONNECT_MAX": "1s"}},
		{name: "duplicate labels", env: map[string]string{"LOOP_RUNNER_LABELS": "linux,linux"}},
		{name: "invalid allowed environment", env: map[string]string{"LOOP_RUNNER_ALLOWED_ENVIRONMENT": "not-safe"}},
		{name: "duplicate allowed environment", env: map[string]string{"LOOP_RUNNER_ALLOWED_ENVIRONMENT": "TOKEN,TOKEN"}},
		{name: "unsafe labels", env: map[string]string{"LOOP_RUNNER_LABELS": "linux,\nroot"}},
		{name: "invalid minimum disk", env: map[string]string{"LOOP_RUNNER_MINIMUM_FREE_BYTES": "0"}},
		{name: "unsafe redaction prefix", env: map[string]string{"LOOP_RUNNER_REDACTION_PREFIXES": "safe,bad\nsuffix"}},
		{name: "invalid Docker enabled", env: map[string]string{"LOOP_RUNNER_DOCKER_ENABLED": "sometimes"}},
		{name: "invalid retention", env: map[string]string{"LOOP_RUNNER_RETAIN_WORKSPACES": "completed"}},
		{name: "duplicate retention", env: map[string]string{"LOOP_RUNNER_RETAIN_WORKSPACES": "failed,failed"}},
		{name: "invalid agent backend", env: map[string]string{"LOOP_RUNNER_AGENT_BACKEND": "unknown"}},
		{name: "missing CLI binary", env: map[string]string{"LOOP_RUNNER_AGENT_BACKEND": "cli"}},
		{name: "missing Docker image", env: map[string]string{"LOOP_RUNNER_AGENT_BACKEND": "docker"}},
		{name: "invalid agent argument", env: map[string]string{"LOOP_RUNNER_AGENT_ARGUMENTS": "safe,bad\nvalue"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Load(lookup(test.env), func() (string, error) { return "runner", nil }); err == nil {
				t.Fatal("Load() accepted invalid configuration")
			}
		})
	}
	if _, err := Load(lookup(nil), func() (string, error) { return "", errors.New("unavailable") }); err == nil {
		t.Fatal("Load() accepted a hostname error")
	}
}

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadRejectsInvalidRetentionBounds(t *testing.T) {
	for name, environment := range map[string]map[string]string{
		"zero age":          {"LOOP_RUNNER_RETENTION_MAX_AGE": "0s"},
		"negative age":      {"LOOP_RUNNER_RETENTION_MAX_AGE": "-1h"},
		"unparsable age":    {"LOOP_RUNNER_RETENTION_MAX_AGE": "forever"},
		"zero count":        {"LOOP_RUNNER_RETENTION_MAX_WORKSPACES": "0"},
		"unparsable count":  {"LOOP_RUNNER_RETENTION_MAX_WORKSPACES": "many"},
		"invalid wip push":  {"LOOP_RUNNER_PUSH_WORK_IN_PROGRESS": "sometimes"},
		"none with another": {"LOOP_RUNNER_RETAIN_WORKSPACES": "none,failed"},
	} {
		if _, err := Load(lookup(environment), func() (string, error) { return "runner", nil }); err == nil {
			t.Fatalf("Load() accepted %s", name)
		}
	}
}
