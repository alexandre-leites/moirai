package control

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type tlsRunnerControlServer struct {
	runnerv1.UnimplementedRunnerControlServer
	unaryHeaders  chan metadata.MD
	streamHeaders chan metadata.MD
}

func (server tlsRunnerControlServer) RegisterRunner(ctx context.Context, _ *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
	if server.unaryHeaders != nil {
		headers, _ := metadata.FromIncomingContext(ctx)
		server.unaryHeaders <- headers
	}
	return &runnerv1.RegisterRunnerResponse{RunnerId: "runner-1", Credential: "credential"}, nil
}

func (server tlsRunnerControlServer) Connect(stream runnerv1.RunnerControl_ConnectServer) error {
	if server.streamHeaders != nil {
		headers, _ := metadata.FromIncomingContext(stream.Context())
		server.streamHeaders <- headers
	}
	_, err := stream.Recv()
	return err
}

func TestDialConnectsToMutualTLSOrchestrator(t *testing.T) {
	caPEM, ca, caKey := testCertificateAuthority(t)
	serverCertificate := testCertificate(t, ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, "orchestrator.test")
	clientCertificate := testCertificate(t, ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, "")
	directory := t.TempDir()
	caPath := writeCertificate(t, directory, "ca.pem", caPEM)
	clientCertPath := writeCertificate(t, directory, "client.pem", clientCertificate.certPEM)
	clientKeyPath := writeCertificate(t, directory, "client-key.pem", clientCertificate.keyPEM)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate.tls}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runnerv1.RegisterRunnerControlServer(server, tlsRunnerControlServer{})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, connection, err := Dial(ctx, listener.Addr().String(), TLSOptions{
		Enabled: true, CAFile: caPath, ClientCertFile: clientCertPath, ClientKeyFile: clientKeyPath, ServerName: "orchestrator.test",
	})
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer connection.Close()
	response, err := client.RegisterRunner(ctx, &runnerv1.RegisterRunnerRequest{ProtocolVersion: "1.0", Name: "runner", Token: "token"})
	if err != nil {
		t.Fatalf("RegisterRunner() error = %v", err)
	}
	if response.GetRunnerId() != "runner-1" {
		t.Fatalf("runner ID = %q", response.GetRunnerId())
	}
}

func TestDialWithHeadersSendsFreshMetadataOnUnaryAndReconnect(t *testing.T) {
	caPEM, ca, caKey := testCertificateAuthority(t)
	serverCertificate := testCertificate(t, ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, "orchestrator.test")
	clientCertificate := testCertificate(t, ca, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, "")
	directory := t.TempDir()
	caPath := writeCertificate(t, directory, "ca.pem", caPEM)
	clientCertPath := writeCertificate(t, directory, "client.pem", clientCertificate.certPEM)
	clientKeyPath := writeCertificate(t, directory, "client-key.pem", clientCertificate.keyPEM)
	headerPath := filepath.Join(directory, "headers.json")
	writeHeaders(t, headerPath, "first-secret")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverCertificate.tls}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	unaryHeaders := make(chan metadata.MD, 1)
	streamHeaders := make(chan metadata.MD, 2)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	runnerv1.RegisterRunnerControlServer(server, tlsRunnerControlServer{unaryHeaders: unaryHeaders, streamHeaders: streamHeaders})
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, connection, err := DialWithHeaders(ctx, listener.Addr().String(), TLSOptions{
		Enabled: true, CAFile: caPath, ClientCertFile: clientCertPath, ClientKeyFile: clientKeyPath, ServerName: "orchestrator.test",
	}, HeaderOptions{File: headerPath})
	if err != nil {
		t.Fatalf("DialWithHeaders() error = %v", err)
	}
	defer connection.Close()
	if _, err := client.RegisterRunner(ctx, &runnerv1.RegisterRunnerRequest{ProtocolVersion: "1.0", Name: "runner", Token: "token"}); err != nil {
		t.Fatalf("RegisterRunner() error = %v", err)
	}
	assertHeaders(t, <-unaryHeaders, "first-secret")
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := stream.Send(&runnerv1.RunnerToOrchestrator{}); err != nil {
		t.Fatalf("stream.Send() error = %v", err)
	}
	assertHeaders(t, <-streamHeaders, "first-secret")

	writeHeaders(t, headerPath, "rotated-secret")
	stream, err = client.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect Connect() error = %v", err)
	}
	if err := stream.Send(&runnerv1.RunnerToOrchestrator{}); err != nil {
		t.Fatalf("reconnect stream.Send() error = %v", err)
	}
	assertHeaders(t, <-streamHeaders, "rotated-secret")
}

func writeHeaders(t *testing.T, path, secret string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"CF-Access-Client-Id":"client.access","CF-Access-Client-Secret":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertHeaders(t *testing.T, headers metadata.MD, secret string) {
	t.Helper()
	if len(headers.Get("cf-access-client-id")) != 1 || len(headers.Get("cf-access-client-secret")) != 1 || headers.Get("cf-access-client-id")[0] != "client.access" || headers.Get("cf-access-client-secret")[0] != secret {
		t.Fatal("headers did not match expected values")
	}
}

type generatedCertificate struct {
	tls     tls.Certificate
	certPEM []byte
	keyPEM  []byte
}

func testCertificateAuthority(t *testing.T) ([]byte, *x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificate, key
}

func testCertificate(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, usages []x509.ExtKeyUsage, serverName string) generatedCertificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: usages, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	if serverName != "" {
		template.DNSNames = []string{serverName}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return generatedCertificate{tls: certificate, certPEM: certPEM, keyPEM: keyPEM}
}

func writeCertificate(t *testing.T, directory, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
