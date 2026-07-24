package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/config"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/dispatch"
	"github.com/loop-engineering/runner/internal/health"
	"github.com/loop-engineering/runner/internal/repository"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if handled, err := probe(os.Args[1:]); handled {
		if err != nil {
			slog.Error("runner probe failed")
			os.Exit(1)
		}
		fmt.Printf("{\"status\":%q}\n", os.Args[1])
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("runner stopped")
		os.Exit(1)
	}
}

type reloadableStreamSettings struct {
	mu       sync.RWMutex
	settings control.StreamSettings
}

func newReloadableStreamSettings(settings config.Config) *reloadableStreamSettings {
	return &reloadableStreamSettings{settings: control.StreamSettings{Labels: append([]string(nil), settings.Labels...), HeartbeatInterval: settings.HeartbeatInterval, ReconnectMin: settings.ReconnectMin, ReconnectMax: settings.ReconnectMax}}
}

func (settings *reloadableStreamSettings) Get() control.StreamSettings {
	settings.mu.RLock()
	defer settings.mu.RUnlock()
	value := settings.settings
	value.Labels = append([]string(nil), value.Labels...)
	return value
}

func (settings *reloadableStreamSettings) Reload(next config.Config) {
	settings.mu.Lock()
	settings.settings = control.StreamSettings{Labels: append([]string(nil), next.Labels...), HeartbeatInterval: next.HeartbeatInterval, ReconnectMin: next.ReconnectMin, ReconnectMax: next.ReconnectMax}
	settings.mu.Unlock()
}

func agentRequiresDocker(settings config.Config) bool {
	return settings.DockerEnabled || settings.AgentBackend == "docker"
}

func agentBinary(settings config.Config) string {
	if settings.AgentBackend == "cli" {
		return settings.AgentBinary
	}
	if settings.AgentBackend == "docker" {
		return "docker"
	}
	return "opencode"
}

func agentBackend(settings config.Config) agents.Backend {
	if settings.AgentBackend == "cli" {
		return agents.CLIBackend{NameValue: "cli", Binary: settings.AgentBinary, Arguments: settings.AgentArguments}
	}
	if settings.AgentBackend == "docker" {
		return agents.DockerCLIBackend{Image: settings.AgentDockerImage, Arguments: settings.AgentArguments}
	}
	return agents.OpenCodeBackend{Arguments: settings.AgentArguments}
}

func probe(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	if len(arguments) != 1 || (arguments[0] != "live" && arguments[0] != "ready") {
		return true, errors.New("runner probe must be live or ready")
	}
	if arguments[0] == "live" {
		return true, nil
	}
	settings, err := config.LoadFromEnvironment()
	if err != nil {
		return true, err
	}
	return true, checkPrerequisites(settings)
}

func checkPrerequisites(settings config.Config) error {
	if err := health.CheckWithBackend(settings.DataDir, agentRequiresDocker(settings), agentBinary(settings), health.DefaultDependencies()); err != nil {
		return fmt.Errorf("runner prerequisites: %w", err)
	}
	if err := health.RequireFreeBytes(settings.DataDir, settings.MinimumFreeBytes, health.AvailableBytes); err != nil {
		return fmt.Errorf("runner disk health: %w", err)
	}
	return nil
}

func retentionPolicy(values []string) dispatch.RetentionPolicy {
	policy := dispatch.RetentionPolicy{}
	for _, value := range values {
		switch value {
		case "succeeded":
			policy.KeepSucceeded = true
		case "failed":
			policy.KeepFailed = true
		case "abandoned":
			policy.KeepAbandoned = true
		}
	}
	return policy
}

func run(ctx context.Context) error {
	settings, err := config.LoadFromEnvironment()
	if err != nil {
		return fmt.Errorf("load runner configuration: %w", err)
	}
	if err := checkPrerequisites(settings); err != nil {
		return err
	}
	if err := agents.ReconcileManifests(settings.DataDir); err != nil {
		return fmt.Errorf("reconcile runner executions: %w", err)
	}
	service, connection, err := control.Dial(ctx, settings.OrchestratorEndpoint, control.TLSOptions{
		Enabled:        settings.TLS,
		CAFile:         settings.TLSCAFile,
		ClientCertFile: settings.TLSClientCertFile,
		ClientKeyFile:  settings.TLSClientKeyFile,
		ServerName:     settings.TLSServerName,
	})
	if err != nil {
		return fmt.Errorf("dial orchestrator: %w", err)
	}
	defer connection.Close()
	identity, err := control.LoadOrRegister(
		ctx,
		control.IdentityStore{Path: settings.IdentityPath()},
		control.NewGRPCService(service),
		settings.RegistrationToken,
		settings.RunnerName,
		settings.Labels,
	)
	if err != nil {
		return fmt.Errorf("bootstrap runner identity: %w", err)
	}
	client, err := control.NewClientFromGRPC(service, identity)
	if err != nil {
		return fmt.Errorf("create runner control client: %w", err)
	}
	projects := dispatch.NewProjectConcurrencyGuard()
	loop, err := dispatch.NewControlLoopWithOutbox(
		client,
		dispatch.Dispatcher{
			Workspaces:         repository.Manager{DataDirectory: settings.DataDir},
			RevisionInspector:  repository.Manager{DataDirectory: settings.DataDir},
			Backend:            agentBackend(settings),
			AllowedEnvironment: settings.AllowedEnvironment,
			MinimumFreeBytes:   settings.MinimumFreeBytes,
			DiskPath:           settings.DataDir,
			AvailableBytes:     health.AvailableBytes,
			Retention:          retentionPolicy(settings.WorkspaceRetention),
			Projects:           projects,
		},
		time.Now,
		60*time.Second,
		15*time.Second,
		settings.RedactionPrefixes,
		settings.EventOutboxPath(),
	)
	if err != nil {
		return fmt.Errorf("create runner dispatch loop: %w", err)
	}
	streamSettings := newReloadableStreamSettings(settings)
	reloadSignal := make(chan os.Signal, 1)
	signal.Notify(reloadSignal, syscall.SIGHUP)
	defer signal.Stop(reloadSignal)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadSignal:
				next, err := config.LoadFromEnvironment()
				if err != nil {
					slog.Warn("runner configuration reload rejected", "runner_id", identity.RunnerID)
					continue
				}
				streamSettings.Reload(next)
				slog.Info("runner control settings reloaded", "runner_id", identity.RunnerID)
			}
		}
	}()
	go func() {
		if err := loop.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("runner dispatch loop stopped", "runner_id", identity.RunnerID)
		}
	}()
	slog.Info("runner initialized", "runner_id", identity.RunnerID, "orchestrator_endpoint", settings.OrchestratorEndpoint)
	return control.StreamSupervisor{
		Client:            client,
		Labels:            settings.Labels,
		HeartbeatInterval: settings.HeartbeatInterval,
		ReconnectMin:      settings.ReconnectMin,
		ReconnectMax:      settings.ReconnectMax,
		Busy:              loop.Busy,
		OnConnected:       loop.FlushEvents,
		OnHeartbeat:       loop.Reconcile,
		Settings:          streamSettings.Get,
	}.Run(ctx)
}
