package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

const (
	defaultDataDir           = "/data"
	defaultHeartbeatInterval = 10 * time.Second
	defaultReconnectMin      = time.Second
	defaultReconnectMax      = time.Minute
)

type Config struct {
	OrchestratorEndpoint string
	DataDir              string
	RunnerName           string
	Labels               []string
	AllowedEnvironment   []string
	RegistrationToken    string
	HeartbeatInterval    time.Duration
	ReconnectMin         time.Duration
	ReconnectMax         time.Duration
	TLS                  bool
	TLSCAFile            string
	TLSClientCertFile    string
	TLSClientKeyFile     string
	TLSServerName        string
	MinimumFreeBytes     uint64
	Capacity             int
	RedactionPrefixes    []string
	DockerEnabled        bool
	WorkspaceRetention   []string
	AgentBackend         string
	AgentBinary          string
	AgentArguments       []string
	AgentDockerImage     string
	MetricsBind          string
}

func (c Config) IdentityPath() string {
	return filepath.Join(c.DataDir, "identity", "runner.json")
}

func (c Config) EventOutboxPath() string {
	return filepath.Join(c.DataDir, "outbox", "events.json")
}

func Load(lookupEnv func(string) (string, bool), hostname func() (string, error)) (Config, error) {
	if lookupEnv == nil || hostname == nil {
		return Config{}, errors.New("runner configuration dependencies are required")
	}
	name, err := hostname()
	if err != nil {
		return Config{}, fmt.Errorf("get runner hostname: %w", err)
	}
	config := Config{
		OrchestratorEndpoint: envOrDefault(lookupEnv, "LOOP_ORCHESTRATOR_ENDPOINT", "orchestrator:50051"),
		DataDir:              envOrDefault(lookupEnv, "LOOP_RUNNER_DATA_DIR", defaultDataDir),
		RunnerName:           envOrDefault(lookupEnv, "LOOP_RUNNER_NAME", name),
		RegistrationToken:    envValue(lookupEnv, "LOOP_RUNNER_REGISTRATION_TOKEN"),
		HeartbeatInterval:    defaultHeartbeatInterval,
		ReconnectMin:         defaultReconnectMin,
		ReconnectMax:         defaultReconnectMax,
		MinimumFreeBytes:     1 << 30,
		Capacity:             1,
		AgentBackend:         envOrDefault(lookupEnv, "LOOP_RUNNER_AGENT_BACKEND", "opencode"),
		AgentBinary:          envValue(lookupEnv, "LOOP_RUNNER_AGENT_BINARY"),
		AgentDockerImage:     envValue(lookupEnv, "LOOP_RUNNER_AGENT_DOCKER_IMAGE"),
		TLSCAFile:            envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CA_FILE"),
		TLSClientCertFile:    envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE"),
		TLSClientKeyFile:     envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE"),
		TLSServerName:        envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_SERVER_NAME"),
		MetricsBind:          envOrDefault(lookupEnv, "LOOP_RUNNER_METRICS_BIND", ":9091"),
	}
	if config.Labels, err = parseLabels(envValue(lookupEnv, "LOOP_RUNNER_LABELS")); err != nil {
		return Config{}, err
	}
	if config.AllowedEnvironment, err = parseAllowedEnvironment(envValue(lookupEnv, "LOOP_RUNNER_ALLOWED_ENVIRONMENT")); err != nil {
		return Config{}, err
	}
	if config.RedactionPrefixes, err = parseRedactionPrefixes(envValue(lookupEnv, "LOOP_RUNNER_REDACTION_PREFIXES")); err != nil {
		return Config{}, err
	}
	if config.WorkspaceRetention, err = parseWorkspaceRetention(envValue(lookupEnv, "LOOP_RUNNER_RETAIN_WORKSPACES")); err != nil {
		return Config{}, err
	}
	if config.AgentArguments, err = parseAgentArguments(envValue(lookupEnv, "LOOP_RUNNER_AGENT_ARGUMENTS")); err != nil {
		return Config{}, err
	}
	if config.HeartbeatInterval, err = durationEnv(lookupEnv, "LOOP_RUNNER_HEARTBEAT_INTERVAL", config.HeartbeatInterval); err != nil {
		return Config{}, err
	}
	if config.ReconnectMin, err = durationEnv(lookupEnv, "LOOP_RUNNER_RECONNECT_MIN", config.ReconnectMin); err != nil {
		return Config{}, err
	}
	if config.ReconnectMax, err = durationEnv(lookupEnv, "LOOP_RUNNER_RECONNECT_MAX", config.ReconnectMax); err != nil {
		return Config{}, err
	}
	if config.TLS, err = boolEnv(lookupEnv, "LOOP_ORCHESTRATOR_TLS", false); err != nil {
		return Config{}, err
	}
	if config.DockerEnabled, err = boolEnv(lookupEnv, "LOOP_RUNNER_DOCKER_ENABLED", false); err != nil {
		return Config{}, err
	}
	if config.MinimumFreeBytes, err = uint64Env(lookupEnv, "LOOP_RUNNER_MINIMUM_FREE_BYTES", config.MinimumFreeBytes); err != nil {
		return Config{}, err
	}
	if config.Capacity, err = intEnv(lookupEnv, "LOOP_RUNNER_CAPACITY", config.Capacity); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func LoadFromEnvironment() (Config, error) {
	return Load(os.LookupEnv, os.Hostname)
}

func (c Config) Validate() error {
	if hasUnsafeText(c.OrchestratorEndpoint) {
		return errors.New("runner orchestrator endpoint is invalid")
	}
	if _, _, err := net.SplitHostPort(c.OrchestratorEndpoint); err != nil {
		return errors.New("runner orchestrator endpoint must include a host and port")
	}
	if _, _, err := net.SplitHostPort(c.MetricsBind); err != nil {
		return errors.New("runner metrics bind must include a host and port")
	}
	if hasUnsafeText(c.DataDir) || !filepath.IsAbs(c.DataDir) {
		return errors.New("runner data directory must be an absolute safe path")
	}
	if hasUnsafeText(c.RunnerName) {
		return errors.New("runner name is invalid")
	}
	if c.HeartbeatInterval <= 0 || c.ReconnectMin <= 0 || c.ReconnectMax < c.ReconnectMin {
		return errors.New("runner timing configuration is invalid")
	}
	if err := validateTLS(c); err != nil {
		return err
	}
	if c.AgentBackend != "opencode" && c.AgentBackend != "cli" && c.AgentBackend != "docker" {
		return errors.New("runner agent backend is invalid")
	}
	if c.AgentBackend == "cli" && (c.AgentBinary == "" || hasUnsafeText(c.AgentBinary)) {
		return errors.New("runner CLI agent binary is invalid")
	}
	if c.AgentBackend == "docker" && (c.AgentDockerImage == "" || hasUnsafeText(c.AgentDockerImage)) {
		return errors.New("runner Docker agent image is invalid")
	}
	if c.Capacity < 1 {
		return errors.New("runner capacity must be a positive integer")
	}
	return nil
}

func validateTLS(config Config) error {
	configured := config.TLSCAFile != "" || config.TLSClientCertFile != "" || config.TLSClientKeyFile != "" || config.TLSServerName != ""
	if configured && !config.TLS {
		return errors.New("runner TLS options require TLS")
	}
	for _, path := range []string{config.TLSCAFile, config.TLSClientCertFile, config.TLSClientKeyFile} {
		if path != "" && (!filepath.IsAbs(path) || hasUnsafeText(path)) {
			return errors.New("runner TLS file path is invalid")
		}
	}
	if (config.TLSClientCertFile == "") != (config.TLSClientKeyFile == "") {
		return errors.New("runner TLS client certificate and key must be configured together")
	}
	if config.TLSServerName != "" && (hasUnsafeText(config.TLSServerName) || len(config.TLSServerName) > 255) {
		return errors.New("runner TLS server name is invalid")
	}
	return nil
}

func envOrDefault(lookupEnv func(string) (string, bool), key, defaultValue string) string {
	value := envValue(lookupEnv, key)
	if value == "" {
		return defaultValue
	}
	return value
}

func envValue(lookupEnv func(string) (string, bool), key string) string {
	value, ok := lookupEnv(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func boolEnv(lookupEnv func(string) (string, bool), key string, defaultValue bool) (bool, error) {
	value, ok := lookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return parsed, nil
}

func uint64Env(lookupEnv func(string) (string, bool), key string, defaultValue uint64) (uint64, error) {
	value, ok := lookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func intEnv(lookupEnv func(string) (string, bool), key string, defaultValue int) (int, error) {
	value, ok := lookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func durationEnv(lookupEnv func(string) (string, bool), key string, defaultValue time.Duration) (time.Duration, error) {
	value, ok := lookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return defaultValue, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

func parseAgentArguments(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	arguments := strings.Split(value, ",")
	if len(arguments) > 32 {
		return nil, errors.New("runner agent arguments exceed limit")
	}
	result := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		argument = strings.TrimSpace(argument)
		if argument == "" || strings.ContainsAny(argument, "\x00\r\n") || len(argument) > 1024 {
			return nil, errors.New("runner agent argument is invalid")
		}
		result = append(result, argument)
	}
	return result, nil
}

func parseWorkspaceRetention(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	retention := strings.Split(value, ",")
	result := make([]string, 0, len(retention))
	seen := make(map[string]struct{}, len(retention))
	for _, status := range retention {
		status = strings.TrimSpace(status)
		if status != "succeeded" && status != "failed" && status != "abandoned" {
			return nil, errors.New("runner workspace retention value is invalid")
		}
		if _, exists := seen[status]; exists {
			return nil, errors.New("runner workspace retention values must be unique")
		}
		seen[status] = struct{}{}
		result = append(result, status)
	}
	return result, nil
}

func parseRedactionPrefixes(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	prefixes := strings.Split(value, ",")
	result := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" || strings.IndexFunc(prefix, unicode.IsControl) >= 0 || len(prefix) > 128 {
			return nil, errors.New("runner redaction prefix is invalid")
		}
		result = append(result, prefix)
	}
	return result, nil
}

func parseAllowedEnvironment(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	values := strings.Split(value, ",")
	allowed := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !environmentName.MatchString(value) {
			return nil, errors.New("runner allowed environment name is invalid")
		}
		if _, exists := seen[value]; exists {
			return nil, errors.New("runner allowed environment names must be unique")
		}
		seen[value] = struct{}{}
		allowed = append(allowed, value)
	}
	return allowed, nil
}

func parseLabels(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return nil, errors.New("runner label is invalid")
	}
	labels := strings.Split(value, ",")
	seen := make(map[string]struct{}, len(labels))
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if hasUnsafeText(label) || len(label) > 128 {
			return nil, errors.New("runner label is invalid")
		}
		if _, exists := seen[label]; exists {
			return nil, errors.New("runner labels must be unique")
		}
		seen[label] = struct{}{}
		result = append(result, label)
	}
	return result, nil
}

func hasUnsafeText(value string) bool {
	return value == "" || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0
}
