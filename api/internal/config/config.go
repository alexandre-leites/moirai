package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// defaultTrustedProxies covers the private address ranges the API's immediate peer
// occupies in the shipped Compose topology (nginx sits on an internal Docker network).
// Operators fronting the API with a different reverse proxy, or exposing it directly to
// untrusted networks, should set LOOP_API_TRUSTED_PROXIES explicitly (empty to disable).
const defaultTrustedProxies = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128"

type Config struct {
	BindAddress               string
	OrchestratorEndpoint      string
	CookieSecure              bool
	CookieKey                 string
	MaxBodyBytes              int64
	TrustedProxies            []*net.IPNet
	OrchestratorTLS           bool
	OrchestratorTLSCAFile     string
	OrchestratorTLSServerName string
}

func FromEnvironment() (Config, error) {
	cfg := Config{
		BindAddress:               envOrDefault("LOOP_API_BIND", ":8080"),
		OrchestratorEndpoint:      envOrDefault("LOOP_ORCHESTRATOR_ENDPOINT", "orchestrator:50051"),
		CookieSecure:              true,
		CookieKey:                 os.Getenv("LOOP_API_COOKIE_KEY"),
		MaxBodyBytes:              1 << 20,
		OrchestratorTLSCAFile:     os.Getenv("LOOP_ORCHESTRATOR_TLS_CA_FILE"),
		OrchestratorTLSServerName: os.Getenv("LOOP_ORCHESTRATOR_TLS_SERVER_NAME"),
	}
	if value, ok := os.LookupEnv("LOOP_API_COOKIE_SECURE"); ok && value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("LOOP_API_COOKIE_SECURE must be a boolean")
		}
		cfg.CookieSecure = parsed
	}
	if value, ok := os.LookupEnv("LOOP_ORCHESTRATOR_TLS"); ok && value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, errors.New("LOOP_ORCHESTRATOR_TLS must be a boolean")
		}
		cfg.OrchestratorTLS = parsed
	}
	if value, ok := os.LookupEnv("LOOP_API_MAX_BODY_BYTES"); ok && value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, errors.New("LOOP_API_MAX_BODY_BYTES must be an integer")
		}
		cfg.MaxBodyBytes = parsed
	}
	trustedProxiesValue := defaultTrustedProxies
	if value, ok := os.LookupEnv("LOOP_API_TRUSTED_PROXIES"); ok {
		trustedProxiesValue = value
	}
	trustedProxies, err := parseTrustedProxies(trustedProxiesValue)
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = trustedProxies
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
	if !c.OrchestratorTLS && (c.OrchestratorTLSCAFile != "" || c.OrchestratorTLSServerName != "") {
		return errors.New("orchestrator TLS options require LOOP_ORCHESTRATOR_TLS")
	}
	return nil
}

func parseTrustedProxies(value string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			ip := net.ParseIP(entry)
			if ip == nil {
				return nil, fmt.Errorf("LOOP_API_TRUSTED_PROXIES contains an invalid address: %q", entry)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			entry = fmt.Sprintf("%s/%d", entry, bits)
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("LOOP_API_TRUSTED_PROXIES contains an invalid network: %q", entry)
		}
		networks = append(networks, network)
	}
	return networks, nil
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
