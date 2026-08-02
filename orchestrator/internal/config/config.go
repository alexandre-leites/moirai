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

	"google.golang.org/grpc/credentials"
)

type Config struct {
	DatabaseURL string
	GRPCBind    string
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
		bind = "0.0.0.0:50051"
	}
	if _, _, err := net.SplitHostPort(bind); err != nil {
		return Config{}, fmt.Errorf("LOOP_GRPC_BIND must be host:port: %w", err)
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
		DatabaseURL:     normalizeDatabaseURL(databaseURL),
		GRPCBind:        bind,
		TLSCertFile:     certFile,
		TLSKeyFile:      keyFile,
		TLSClientCAFile: clientCAFile,
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
