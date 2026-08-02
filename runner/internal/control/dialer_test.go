package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransportCredentialsRejectsInvalidTLSOptions(t *testing.T) {
	tests := []TLSOptions{
		{CAFile: "/tmp/ca.pem"},
		{Enabled: true, ClientCertFile: "/tmp/client.pem"},
		{Enabled: true, ClientKeyFile: "/tmp/client-key.pem"},
	}
	for _, options := range tests {
		if _, err := transportCredentials(options); err == nil {
			t.Fatalf("transportCredentials(%#v) succeeded", options)
		}
	}
}

func TestDialWithHeadersRequiresTLSWithoutLeakingHeaderValue(t *testing.T) {
	secret := "do-not-log-this"
	_, _, err := DialWithHeaders(context.Background(), "127.0.0.1:1", TLSOptions{}, HeaderOptions{Headers: map[string]string{"x-proxy-token": secret}})
	if err == nil || !strings.Contains(err.Error(), "require TLS") || strings.Contains(err.Error(), secret) {
		t.Fatalf("DialWithHeaders() error = %v", err)
	}
}

func TestHeaderCredentialsDoNotExposeFileContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.json")
	secret := "do-not-log-this"
	if err := os.WriteFile(path, []byte(`{"x-proxy-token":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := headerCredentials{options: HeaderOptions{File: path}}
	headers, err := credentials.GetRequestMetadata(context.Background())
	if err != nil || headers["x-proxy-token"] != secret {
		t.Fatal("GetRequestMetadata() did not return expected header")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.GetRequestMetadata(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("GetRequestMetadata() error = %v", err)
	}
}

func TestTransportCredentialsUsesConfiguredCA(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := transportCredentials(TLSOptions{Enabled: true, CAFile: path, ServerName: "control.internal"}); err == nil {
		t.Fatal("transportCredentials() accepted invalid CA data")
	}
	if _, err := transportCredentials(TLSOptions{Enabled: true, ServerName: "control.internal"}); err != nil {
		t.Fatalf("transportCredentials() error = %v", err)
	}
}
