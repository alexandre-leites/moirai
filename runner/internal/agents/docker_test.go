package agents

import (
	"context"
	"testing"
)

func TestDockerCLIBackendRequiresImage(t *testing.T) {
	if err := (DockerCLIBackend{}).HealthCheck(context.Background()); err == nil {
		t.Fatal("HealthCheck() accepted missing image")
	}
	if err := (DockerCLIBackend{Image: "example/agent:1"}).HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck() error = %v", err)
	}
}
