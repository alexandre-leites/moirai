package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type TLSOptions struct {
	Enabled        bool
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	ServerName     string
}

type HeaderOptions struct {
	Headers map[string]string
	File    string
}

type headerCredentials struct {
	options HeaderOptions
}

func (c headerCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if c.options.File != "" {
		headers, err := config.ReadOrchestratorHeaders(c.options.File)
		if err != nil {
			return nil, errors.New("runner orchestrator headers are unavailable")
		}
		return headers, nil
	}
	return c.options.Headers, nil
}

func (headerCredentials) RequireTransportSecurity() bool {
	return true
}

func Dial(ctx context.Context, endpoint string, tlsOptions TLSOptions) (runnerv1.RunnerControlClient, *grpc.ClientConn, error) {
	return DialWithHeaders(ctx, endpoint, tlsOptions, HeaderOptions{})
}

func DialWithHeaders(ctx context.Context, endpoint string, tlsOptions TLSOptions, headerOptions HeaderOptions) (runnerv1.RunnerControlClient, *grpc.ClientConn, error) {
	if endpoint == "" {
		return nil, nil, errors.New("runner orchestrator endpoint is required")
	}
	transport, err := transportCredentials(tlsOptions)
	if err != nil {
		return nil, nil, err
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	if len(headerOptions.Headers) > 0 || headerOptions.File != "" {
		if !tlsOptions.Enabled {
			return nil, nil, errors.New("runner orchestrator headers require TLS because credentials must not be sent over an insecure connection")
		}
		credentials := headerCredentials{options: headerOptions}
		if _, err := credentials.GetRequestMetadata(ctx); err != nil {
			return nil, nil, err
		}
		options = append(options, grpc.WithPerRPCCredentials(credentials))
	}
	connection, err := grpc.DialContext(ctx, endpoint, options...)
	if err != nil {
		return nil, nil, err
	}
	return runnerv1.NewRunnerControlClient(connection), connection, nil
}

func transportCredentials(options TLSOptions) (credentials.TransportCredentials, error) {
	if !options.Enabled {
		if options.CAFile != "" || options.ClientCertFile != "" || options.ClientKeyFile != "" || options.ServerName != "" {
			return nil, errors.New("runner TLS options require TLS")
		}
		return insecure.NewCredentials(), nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: options.ServerName}
	if options.CAFile != "" {
		contents, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read runner TLS CA file: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(contents) {
			return nil, errors.New("runner TLS CA file contains no certificates")
		}
		config.RootCAs = roots
	}
	if (options.ClientCertFile == "") != (options.ClientKeyFile == "") {
		return nil, errors.New("runner TLS client certificate and key must be configured together")
	}
	if options.ClientCertFile != "" {
		certificate, err := tls.LoadX509KeyPair(options.ClientCertFile, options.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load runner TLS client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return credentials.NewTLS(config), nil
}
