package control

import (
	"os"
	"path/filepath"
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
