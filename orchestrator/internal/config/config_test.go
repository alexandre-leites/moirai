package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeKeyPair emits a self-signed certificate and its key, returning both paths.
func writeKeyPair(t *testing.T) (certFile string, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "orchestrator"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encodedKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// loadWith runs Load with the database URL and bind already satisfied so each
// case only has to describe the TLS variables it is actually about.
func loadWith(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	t.Setenv("LOOP_DATABASE_URL", "postgres://loop:secret@postgres/loop")
	t.Setenv("LOOP_GRPC_BIND", "127.0.0.1:50051")
	for _, name := range []string{"LOOP_GRPC_TLS_CERT_FILE", "LOOP_GRPC_TLS_KEY_FILE", "LOOP_GRPC_TLS_CLIENT_CA_FILE"} {
		t.Setenv(name, env[name])
	}
	return Load()
}

func TestLoadWithoutTLSServesPlaintext(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := cfg.ServerCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if transport != nil {
		t.Fatal("ServerCredentials() returned credentials without TLS configured")
	}
}

func TestLoadRejectsHalfConfiguredTLS(t *testing.T) {
	certFile, keyFile := writeKeyPair(t)
	for name, env := range map[string]map[string]string{
		"cert without key": {"LOOP_GRPC_TLS_CERT_FILE": certFile},
		"key without cert": {"LOOP_GRPC_TLS_KEY_FILE": keyFile},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadWith(t, env); err == nil {
				t.Fatal("Load() silently downgraded half-configured TLS to plaintext")
			}
		})
	}
}

func TestLoadRejectsClientCAWithoutServerTLS(t *testing.T) {
	certFile, _ := writeKeyPair(t)
	if _, err := loadWith(t, map[string]string{"LOOP_GRPC_TLS_CLIENT_CA_FILE": certFile}); err == nil {
		t.Fatal("Load() accepted a client CA without server TLS")
	}
}

func TestServerCredentialsFromKeyPair(t *testing.T) {
	certFile, keyFile := writeKeyPair(t)
	cfg, err := loadWith(t, map[string]string{
		"LOOP_GRPC_TLS_CERT_FILE": certFile,
		"LOOP_GRPC_TLS_KEY_FILE":  keyFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := cfg.ServerCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if transport == nil {
		t.Fatal("ServerCredentials() returned no credentials for a configured key pair")
	}
	if got := transport.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("SecurityProtocol = %q, want tls", got)
	}
}

func TestServerCredentialsRequireClientCertificatesWithCA(t *testing.T) {
	certFile, keyFile := writeKeyPair(t)
	cfg, err := loadWith(t, map[string]string{
		"LOOP_GRPC_TLS_CERT_FILE":      certFile,
		"LOOP_GRPC_TLS_KEY_FILE":       keyFile,
		"LOOP_GRPC_TLS_CLIENT_CA_FILE": certFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := cfg.ServerCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if transport == nil {
		t.Fatal("ServerCredentials() returned no credentials for mTLS")
	}
}

func TestServerCredentialsRejectsUnreadableCA(t *testing.T) {
	certFile, keyFile := writeKeyPair(t)
	cfg, err := loadWith(t, map[string]string{
		"LOOP_GRPC_TLS_CERT_FILE":      certFile,
		"LOOP_GRPC_TLS_KEY_FILE":       keyFile,
		"LOOP_GRPC_TLS_CLIENT_CA_FILE": filepath.Join(t.TempDir(), "absent.crt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ServerCredentials(); err == nil {
		t.Fatal("ServerCredentials() accepted an unreadable client CA")
	}
}

func TestLoadNormalizesPythonDatabaseURL(t *testing.T) {
	t.Setenv("LOOP_DATABASE_URL", "postgresql+asyncpg://loop:secret@postgres:5432/loop")
	t.Setenv("LOOP_GRPC_BIND", "127.0.0.1:50051")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseURL != "postgresql://loop:secret@postgres:5432/loop" {
		t.Fatalf("DatabaseURL = %q", got.DatabaseURL)
	}
}

func TestLoadReadsSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database-url")
	if err := os.WriteFile(path, []byte("postgres://loop:secret@postgres/loop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_DATABASE_URL", "")
	t.Setenv("LOOP_DATABASE_URL_FILE", path)
	t.Setenv("LOOP_GRPC_BIND", "127.0.0.1:50051")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabaseURL != "postgres://loop:secret@postgres/loop" {
		t.Fatalf("DatabaseURL = %q", got.DatabaseURL)
	}
}

func TestLoadRejectsAmbiguousSecret(t *testing.T) {
	t.Setenv("LOOP_DATABASE_URL", "postgres://loop:secret@postgres/loop")
	t.Setenv("LOOP_DATABASE_URL_FILE", "database-url")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted both database URL sources")
	}
}
