package config

import (
	"net"
	"testing"
)

func TestFromEnvironmentDefaults(t *testing.T) {
	t.Setenv("LOOP_API_BIND", "")
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", "")
	t.Setenv("LOOP_API_COOKIE_SECURE", "")
	t.Setenv("LOOP_ORCHESTRATOR_TLS", "")
	t.Setenv("LOOP_ORCHESTRATOR_TLS_CA_FILE", "")
	t.Setenv("LOOP_ORCHESTRATOR_TLS_SERVER_NAME", "")
	cfg, err := FromEnvironment()
	if err != nil {
		t.Fatalf("from environment: %v", err)
	}
	if cfg.BindAddress != ":8080" || cfg.OrchestratorEndpoint != "orchestrator:50051" || !cfg.CookieSecure || cfg.MaxBodyBytes != 1<<20 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Fatal("expected default trusted proxies to be populated")
	}
}

func TestFromEnvironmentTrustedProxies(t *testing.T) {
	t.Setenv("LOOP_API_BIND", "127.0.0.1:8080")
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", "orchestrator:50051")

	t.Setenv("LOOP_API_TRUSTED_PROXIES", "203.0.113.5,198.51.100.0/24")
	cfg, err := FromEnvironment()
	if err != nil {
		t.Fatalf("from environment: %v", err)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("expected 2 trusted proxies, got %d", len(cfg.TrustedProxies))
	}
	if !cfg.TrustedProxies[0].Contains(net.ParseIP("203.0.113.5")) {
		t.Fatal("expected bare IP to be normalized to a /32 network")
	}
	if !cfg.TrustedProxies[1].Contains(net.ParseIP("198.51.100.200")) {
		t.Fatal("expected CIDR to be preserved")
	}

	t.Setenv("LOOP_API_TRUSTED_PROXIES", "")
	cfg, err = FromEnvironment()
	if err != nil {
		t.Fatalf("from environment: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("expected explicit empty value to disable trusted proxies, got %#v", cfg.TrustedProxies)
	}

	t.Setenv("LOOP_API_TRUSTED_PROXIES", "not-an-ip")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid trusted proxy error")
	}
}

func TestFromEnvironmentParsesOrchestratorTLS(t *testing.T) {
	t.Setenv("LOOP_ORCHESTRATOR_TLS", "true")
	t.Setenv("LOOP_ORCHESTRATOR_TLS_CA_FILE", "/etc/loop/ca.pem")
	t.Setenv("LOOP_ORCHESTRATOR_TLS_SERVER_NAME", "orchestrator.internal")
	cfg, err := FromEnvironment()
	if err != nil || !cfg.OrchestratorTLS || cfg.OrchestratorTLSCAFile != "/etc/loop/ca.pem" {
		t.Fatalf("unexpected TLS configuration: %#v, %v", cfg, err)
	}
	t.Setenv("LOOP_ORCHESTRATOR_TLS", "false")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected TLS options without TLS to be rejected")
	}
}

func TestFromEnvironmentValidatesAddressesAndCookieFlag(t *testing.T) {
	t.Setenv("LOOP_API_BIND", "invalid")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid bind address error")
	}
	t.Setenv("LOOP_API_BIND", "127.0.0.1:8080")
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", "invalid")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid endpoint error")
	}
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", "orchestrator:50051")
	t.Setenv("LOOP_API_COOKIE_SECURE", "not-a-bool")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid cookie flag error")
	}
	t.Setenv("LOOP_API_COOKIE_SECURE", "false")
	t.Setenv("LOOP_API_MAX_BODY_BYTES", "2048")
	t.Setenv("LOOP_API_COOKIE_KEY", "01234567890123456789012345678901")
	cfg, err := FromEnvironment()
	if err != nil || cfg.CookieSecure || cfg.MaxBodyBytes != 2048 {
		t.Fatalf("expected valid insecure config, got %#v, %v", cfg, err)
	}
	t.Setenv("LOOP_API_MAX_BODY_BYTES", "1")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid body limit error")
	}
	t.Setenv("LOOP_API_MAX_BODY_BYTES", "2048")
	t.Setenv("LOOP_API_COOKIE_KEY", "short")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("expected invalid cookie key error")
	}
}
