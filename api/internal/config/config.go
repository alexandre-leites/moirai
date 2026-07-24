package config

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BindAddress          string
	OrchestratorEndpoint string
	CookieSecure         bool
	CookieKey            string
	MaxBodyBytes         int64
}

func FromEnvironment() (Config, error) {
	cfg := Config{
		BindAddress:          envOrDefault("LOOP_API_BIND", ":8080"),
		OrchestratorEndpoint: envOrDefault("LOOP_ORCHESTRATOR_ENDPOINT", "orchestrator:50051"),
		CookieSecure:         true,
		CookieKey:            os.Getenv("LOOP_API_COOKIE_KEY"),
		MaxBodyBytes:         1 << 20,
	}
	if value, ok := os.LookupEnv("LOOP_API_COOKIE_SECURE"); ok && value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("LOOP_API_COOKIE_SECURE must be a boolean")
		}
		cfg.CookieSecure = parsed
	}
	if value, ok := os.LookupEnv("LOOP_API_MAX_BODY_BYTES"); ok && value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, errors.New("LOOP_API_MAX_BODY_BYTES must be an integer")
		}
		cfg.MaxBodyBytes = parsed
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if !validAddress(c.BindAddress) {
		return errors.New("LOOP_API_BIND must be a host:port address")
	}
	if !validAddress(c.OrchestratorEndpoint) {
		return errors.New("LOOP_ORCHESTRATOR_ENDPOINT must be a host:port address")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 {
		return errors.New("LOOP_API_MAX_BODY_BYTES must be between 1024 and 16777216")
	}
	if c.CookieKey != "" && (len(c.CookieKey) < 32 || strings.ContainsAny(c.CookieKey, "\r\n\x00")) {
		return errors.New("LOOP_API_COOKIE_KEY must be at least 32 printable bytes")
	}
	return nil
}

func validAddress(value string) bool {
	_, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return false
	}
	return true
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
