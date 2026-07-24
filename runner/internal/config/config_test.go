package config

import (
	"errors"
	"testing"
	"time"
)

func TestLoadUsesDefaultsAndParsesRunnerEnvironment(t *testing.T) {
	environment := map[string]string{
		"LOOP_ORCHESTRATOR_ENDPOINT":      "control.example:7443",
		"LOOP_RUNNER_DATA_DIR":            "/var/lib/loop",
		"LOOP_RUNNER_NAME":                "worker-a",
		"LOOP_RUNNER_LABELS":              "linux, docker,opencode",
		"LOOP_RUNNER_ALLOWED_ENVIRONMENT": "GITHUB_TOKEN,PROJECT_KEY",

		"LOOP_RUNNER_REGISTRATION_TOKEN":         "registration-token",
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
		"LOOP_RUNNER_AGENT_ARGUMENTS":            "run,--safe",
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
