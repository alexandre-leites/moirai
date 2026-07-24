package config

import "testing"

func TestFromEnvironmentDefaults(t *testing.T) {
	t.Setenv("LOOP_API_BIND", "")
	t.Setenv("LOOP_ORCHESTRATOR_ENDPOINT", "")
	t.Setenv("LOOP_API_COOKIE_SECURE", "")
	cfg, err := FromEnvironment()
	if err != nil {
		t.Fatalf("from environment: %v", err)
	}
	if cfg.BindAddress != ":8080" || cfg.OrchestratorEndpoint != "orchestrator:50051" || !cfg.CookieSecure || cfg.MaxBodyBytes != 1<<20 {
		t.Fatalf("unexpected defaults: %#v", cfg)
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
