package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
)

// DefaultGRPCBind is the address served when LOOP_GRPC_BIND is unset. The
// healthcheck derives its port from the same value, so both move together.
const DefaultGRPCBind = "0.0.0.0:50051"

// DefaultMetricsBind is the address /metrics is served on when
// LOOP_METRICS_BIND is unset. It is the port the previous orchestrator used, so
// an existing scrape target still resolves — though two of its series were
// renamed on the way (moirai_active_workflow_count and
// moirai_scheduled_job_count; see orchestrator/README.md), so queries written
// against those names need updating. It is distinct from the runner's
// LOOP_RUNNER_METRICS_BIND (`:9091`) so both can run on one host.
const DefaultMetricsBind = "0.0.0.0:9090"

type Config struct {
	DatabaseURL string
	GRPCBind    string
	// MetricsBind is the host:port the Prometheus surface is served on, or
	// empty when LOOP_METRICS_BIND was explicitly set to nothing and the
	// listener is disabled.
	MetricsBind string
	// IssueSyncInterval is how often issues are re-read from the tracker. It is
	// configurable because the useful cadence depends on the tracker's rate
	// limits and on how quickly a team expects a newly labelled issue to be
	// picked up.
	IssueSyncInterval time.Duration
	// TLSCertFile and TLSKeyFile are set together or not at all; when unset the
	// gRPC endpoint is served in plaintext. TLSClientCAFile additionally
	// requires client certificates (mTLS) and is meaningless without them.
	TLSCertFile     string
	TLSKeyFile      string
	TLSClientCAFile string
}

func Load() (Config, error) {
	databaseURL, err := value("LOOP_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	bind := os.Getenv("LOOP_GRPC_BIND")
	if bind == "" {
		bind = DefaultGRPCBind
	}
	if _, _, err := net.SplitHostPort(bind); err != nil {
		return Config{}, fmt.Errorf("LOOP_GRPC_BIND must be host:port: %w", err)
	}
	// Unset means the default, so a deployment that says nothing about metrics
	// still exports them: queue depth and the fleet-wide heartbeat age exist
	// nowhere else, and an observability surface nobody remembered to turn on
	// is one nobody has. Explicitly empty is the way to turn the listener off.
	metricsBind := DefaultMetricsBind
	if configured, set := os.LookupEnv("LOOP_METRICS_BIND"); set {
		metricsBind = strings.TrimSpace(configured)
	}
	if metricsBind != "" {
		// The port is checked separately because SplitHostPort accepts an empty
		// one: "0.0.0.0:" would pass, bind an ephemeral port nothing is
		// configured to scrape, and look healthy while exporting to nobody.
		_, port, err := net.SplitHostPort(metricsBind)
		if err != nil {
			return Config{}, fmt.Errorf("LOOP_METRICS_BIND must be host:port: %w", err)
		}
		if port == "" {
			return Config{}, fmt.Errorf("LOOP_METRICS_BIND must name a port, got %q", metricsBind)
		}
	}
	// Refused rather than warned about, so a typo in the interval fails the same
	// way a typo in the bind address does instead of silently reverting to a
	// cadence the operator did not choose.
	syncInterval := 2 * time.Minute
	if value := strings.TrimSpace(os.Getenv("LOOP_ISSUE_SYNC_INTERVAL")); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return Config{}, fmt.Errorf("LOOP_ISSUE_SYNC_INTERVAL must be a positive duration, got %q", value)
		}
		syncInterval = parsed
	}
	certFile := strings.TrimSpace(os.Getenv("LOOP_GRPC_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("LOOP_GRPC_TLS_KEY_FILE"))
	clientCAFile := strings.TrimSpace(os.Getenv("LOOP_GRPC_TLS_CLIENT_CA_FILE"))
	// Half-configured TLS is refused rather than silently downgraded: an
	// operator who set one of the pair asked for an encrypted endpoint, and
	// serving plaintext instead would be a silent security downgrade.
	if (certFile == "") != (keyFile == "") {
		return Config{}, errors.New("LOOP_GRPC_TLS_CERT_FILE and LOOP_GRPC_TLS_KEY_FILE must be configured together")
	}
	if clientCAFile != "" && certFile == "" {
		return Config{}, errors.New("LOOP_GRPC_TLS_CLIENT_CA_FILE requires server TLS")
	}
	return Config{
		DatabaseURL:       normalizeDatabaseURL(databaseURL),
		GRPCBind:          bind,
		MetricsBind:       metricsBind,
		IssueSyncInterval: syncInterval,
		TLSCertFile:       certFile,
		TLSKeyFile:        keyFile,
		TLSClientCAFile:   clientCAFile,
	}, nil
}

// ServerCredentials returns the gRPC transport credentials for the configured
// endpoint, or nil when TLS is not configured and the endpoint is plaintext.
func (c Config) ServerCredentials() (credentials.TransportCredentials, error) {
	if c.TLSCertFile == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load gRPC TLS keypair: %w", err)
	}
	settings := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	if c.TLSClientCAFile != "" {
		authorities := x509.NewCertPool()
		pem, err := os.ReadFile(c.TLSClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("read LOOP_GRPC_TLS_CLIENT_CA_FILE: %w", err)
		}
		if !authorities.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("LOOP_GRPC_TLS_CLIENT_CA_FILE %q contains no certificates", c.TLSClientCAFile)
		}
		settings.ClientCAs = authorities
		settings.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(settings), nil
}

func value(name string) (string, error) {
	file := os.Getenv(name + "_FILE")
	plain := os.Getenv(name)
	if file != "" && plain != "" {
		return "", fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
	}
	if file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		plain = strings.TrimSpace(string(contents))
	}
	if plain == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return plain, nil
}

func normalizeDatabaseURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}
	if parsed.Scheme == "postgresql+asyncpg" {
		parsed.Scheme = "postgresql"
	}
	return parsed.String()
}
