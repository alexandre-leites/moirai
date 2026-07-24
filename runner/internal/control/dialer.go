package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
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

func Dial(ctx context.Context, endpoint string, tlsOptions TLSOptions) (runnerv1.RunnerControlClient, *grpc.ClientConn, error) {
	if endpoint == "" {
		return nil, nil, errors.New("runner orchestrator endpoint is required")
	}
	transport, err := transportCredentials(tlsOptions)
	if err != nil {
		return nil, nil, err
	}
	connection, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(transport))
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
