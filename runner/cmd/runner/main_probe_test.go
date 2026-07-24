package main

import (
	"testing"
	"time"

	"github.com/loop-engineering/runner/internal/config"
)

func TestProbeHandlesLivenessAndArguments(t *testing.T) {
	handled, err := probe(nil)
	if handled || err != nil {
		t.Fatalf("probe(nil) = (%t, %v)", handled, err)
	}
	handled, err = probe([]string{"live"})
	if !handled || err != nil {
		t.Fatalf("probe(live) = (%t, %v)", handled, err)
	}
	handled, err = probe([]string{"unknown"})
	if !handled || err == nil {
		t.Fatalf("probe(unknown) = (%t, %v)", handled, err)
	}
}

func TestReloadableStreamSettingsAppliesReloadWithoutSharingLabels(t *testing.T) {
	current := newReloadableStreamSettings(config.Config{Labels: []string{"linux"}, HeartbeatInterval: time.Second, ReconnectMin: time.Second, ReconnectMax: time.Minute})
	current.Reload(config.Config{Labels: []string{"linux", "docker"}, HeartbeatInterval: 2 * time.Second, ReconnectMin: 2 * time.Second, ReconnectMax: 2 * time.Minute})
	settings := current.Get()
	if len(settings.Labels) != 2 || settings.Labels[1] != "docker" || settings.HeartbeatInterval != 2*time.Second || settings.ReconnectMin != 2*time.Second || settings.ReconnectMax != 2*time.Minute {
		t.Fatalf("reloaded stream settings = %#v", settings)
	}
	settings.Labels[0] = "modified"
	if current.Get().Labels[0] != "linux" {
		t.Fatal("stream settings labels share mutable state")
	}
}

func TestProbeReadinessFailsClosedOnInvalidConfiguration(t *testing.T) {
	t.Setenv("LOOP_RUNNER_DATA_DIR", "relative")
	handled, err := probe([]string{"ready"})
	if !handled || err == nil {
		t.Fatalf("probe(ready) = (%t, %v)", handled, err)
	}
}
