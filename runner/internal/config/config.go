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
	RetentionMaxAge      time.Duration
	RetentionMaxCount    int
	PushWorkInProgress   bool
	AgentBackend         string
	AgentBinary          string
	AgentArguments       []string
	AgentDockerImage     string
	MetricsBind          string
	GitCommitterName     string
	GitCommitterEmail    string
	LeaseDuration        time.Duration
	LeaseRenewalLead     time.Duration
	OfferTimeout         time.Duration
	ReconnectGrace       time.Duration
	EventBufferSize      int
	EventPayloadBytes    int
	LogChunkBytes        int
	MaxLogBytes          int
	TerminationGrace     time.Duration
	DockerCPULimit       string
	DockerMemoryLimit    string
	DockerNetwork        string
	DockerStopTimeout    time.Duration
	RepositoryLockPoll   time.Duration
	CleanupAttempts      int
	CleanupRetryDelay    time.Duration
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
		HeartbeatInterval:    defaultHeartbeatInterval,
		ReconnectMin:         defaultReconnectMin,
		ReconnectMax:         defaultReconnectMax,
		MinimumFreeBytes:     1 << 30,
		Capacity:             1,
		AgentBackend:         envOrDefault(lookupEnv, "LOOP_RUNNER_AGENT_BACKEND", "opencode"),
		AgentBinary:          envValue(lookupEnv, "LOOP_RUNNER_AGENT_BINARY"),
		AgentDockerImage:     envValue(lookupEnv, "LOOP_RUNNER_AGENT_DOCKER_IMAGE"),
		GitCommitterName:     envOrDefault(lookupEnv, "LOOP_RUNNER_GIT_COMMITTER_NAME", "moirai-runner"),
		GitCommitterEmail:    envOrDefault(lookupEnv, "LOOP_RUNNER_GIT_COMMITTER_EMAIL", "moirai-runner@localhost"),
		LeaseDuration:        time.Minute,
		LeaseRenewalLead:     15 * time.Second,
		OfferTimeout:         30 * time.Second,
		ReconnectGrace:       time.Minute,
		EventBufferSize:      128,
		EventPayloadBytes:    16 * 1024,
		LogChunkBytes:        6 * 1024,
		MaxLogBytes:          4 << 20,
		TerminationGrace:     5 * time.Second,
		DockerNetwork:        "bridge",
		DockerStopTimeout:    10 * time.Second,
		RepositoryLockPoll:   25 * time.Millisecond,
		CleanupAttempts:      3,
		CleanupRetryDelay:    250 * time.Millisecond,
		RetentionMaxAge:      72 * time.Hour,
		RetentionMaxCount:    10,
		TLSCAFile:            envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CA_FILE"),
		TLSClientCertFile:    envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CLIENT_CERT_FILE"),
		TLSClientKeyFile:     envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_CLIENT_KEY_FILE"),
		TLSServerName:        envValue(lookupEnv, "LOOP_ORCHESTRATOR_TLS_SERVER_NAME"),
		MetricsBind:          envOrDefault(lookupEnv, "LOOP_RUNNER_METRICS_BIND", ":9091"),
	}
	if config.RegistrationToken, err = secretFileValue(lookupEnv, "LOOP_RUNNER_REGISTRATION_TOKEN"); err != nil {
		return Config{}, err
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
	for _, value := range []struct {
		key    string
		target *time.Duration
	}{
		{"LOOP_RUNNER_LEASE_DURATION", &config.LeaseDuration},
		{"LOOP_RUNNER_LEASE_RENEWAL_LEAD", &config.LeaseRenewalLead},
		{"LOOP_RUNNER_OFFER_TIMEOUT", &config.OfferTimeout},
		{"LOOP_RUNNER_RECONNECT_GRACE", &config.ReconnectGrace},
		{"LOOP_RUNNER_TERMINATION_GRACE", &config.TerminationGrace},
		{"LOOP_RUNNER_DOCKER_STOP_TIMEOUT", &config.DockerStopTimeout},
		{"LOOP_RUNNER_REPOSITORY_LOCK_POLL", &config.RepositoryLockPoll},
		{"LOOP_RUNNER_CLEANUP_RETRY_DELAY", &config.CleanupRetryDelay},
		{"LOOP_RUNNER_RETENTION_MAX_AGE", &config.RetentionMaxAge},
	} {
		if *value.target, err = durationEnv(lookupEnv, value.key, *value.target); err != nil {
			return Config{}, err
		}
	}
	for _, value := range []struct {
		key    string
		target *int
	}{
		{"LOOP_RUNNER_EVENT_BUFFER_SIZE", &config.EventBufferSize},
		{"LOOP_RUNNER_EVENT_PAYLOAD_BYTES", &config.EventPayloadBytes},
		{"LOOP_RUNNER_LOG_CHUNK_BYTES", &config.LogChunkBytes},
		{"LOOP_RUNNER_MAX_LOG_BYTES", &config.MaxLogBytes},
		{"LOOP_RUNNER_CLEANUP_ATTEMPTS", &config.CleanupAttempts},
		{"LOOP_RUNNER_RETENTION_MAX_WORKSPACES", &config.RetentionMaxCount},
	} {
		if *value.target, err = intEnv(lookupEnv, value.key, *value.target); err != nil {
			return Config{}, err
		}
	}
	config.DockerCPULimit = envValue(lookupEnv, "LOOP_RUNNER_DOCKER_CPU_LIMIT")
	config.DockerMemoryLimit = envValue(lookupEnv, "LOOP_RUNNER_DOCKER_MEMORY_LIMIT")
	config.DockerNetwork = envOrDefault(lookupEnv, "LOOP_RUNNER_DOCKER_NETWORK", config.DockerNetwork)
	if config.TLS, err = boolEnv(lookupEnv, "LOOP_ORCHESTRATOR_TLS", false); err != nil {
		return Config{}, err
	}
	if config.DockerEnabled, err = boolEnv(lookupEnv, "LOOP_RUNNER_DOCKER_ENABLED", false); err != nil {
		return Config{}, err
	}
	if config.PushWorkInProgress, err = boolEnv(lookupEnv, "LOOP_RUNNER_PUSH_WORK_IN_PROGRESS", true); err != nil {
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
	if (c.AgentBackend == "docker" || c.DockerEnabled) && (c.AgentDockerImage == "" || hasUnsafeText(c.AgentDockerImage)) {
		return errors.New("runner Docker agent image is invalid")
	}
	if c.Capacity < 1 {
		return errors.New("runner capacity must be a positive integer")
	}
	if hasUnsafeText(c.GitCommitterName) || hasUnsafeText(c.GitCommitterEmail) || !strings.Contains(c.GitCommitterEmail, "@") {
		return errors.New("runner git committer identity is invalid")
	}
	if c.LeaseRenewalLead >= c.LeaseDuration {
		return errors.New("runner lease renewal lead must be shorter than lease duration")
	}
	if c.EventBufferSize < 1 || c.EventPayloadBytes < 1 || c.LogChunkBytes < 1 || c.MaxLogBytes < 1 || c.CleanupAttempts < 1 {
		return errors.New("runner sizing configuration is invalid")
	}
	if len(c.WorkspaceRetention) > 0 && (c.RetentionMaxAge <= 0 || c.RetentionMaxCount < 1) {
		return errors.New("runner workspace retention must be bounded by an age and a workspace count")
	}
	if hasUnsafeText(c.DockerNetwork) || hasUnsafeText(c.DockerCPULimit) && c.DockerCPULimit != "" || hasUnsafeText(c.DockerMemoryLimit) && c.DockerMemoryLimit != "" {
		return errors.New("runner Docker configuration is invalid")
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

// SecretValue resolves a named secret, preferring the Docker-style
// "<NAME>_FILE" indirection that Compose secrets use over the plain variable.
// It returns an empty value with a nil error when neither is configured, so
// callers can report a missing secret in their own terms.
func SecretValue(lookupEnv func(string) (string, bool), key string) (string, error) {
	value, err := secretFileValue(lookupEnv, key)
	if err != nil || value != "" {
		return value, err
	}
	return envValue(lookupEnv, key), nil
}

func secretFileValue(lookupEnv func(string) (string, bool), key string) (string, error) {
	path := envValue(lookupEnv, key+"_FILE")
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) || hasUnsafeText(path) {
		return "", fmt.Errorf("%s_FILE must be an absolute safe path", key)
	}
	metadata, err := os.Stat(path)
	if err != nil || !metadata.Mode().IsRegular() || metadata.Size() > 16_384 {
		return "", fmt.Errorf("%s_FILE cannot be read", key)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s_FILE cannot be read", key)
	}
	value := strings.TrimSpace(string(contents))
	if value == "" || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s_FILE contains an invalid token", key)
	}
	return value, nil
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

// parseWorkspaceRetention selects which terminal outcomes keep their workspace.
// The default keeps failed runs, so the worktree, terminal result, and agent
// logs of a failure survive it — for inspection, and for a retry that lands on
// the same runner before the job prepares again. Retention is bounded by
// LOOP_RUNNER_RETENTION_MAX_AGE and LOOP_RUNNER_RETENTION_MAX_WORKSPACES, and
// "none" opts out of retention altogether.
func parseWorkspaceRetention(value string) ([]string, error) {
	if value == "" {
		return []string{"failed"}, nil
	}
	if value == "none" {
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
