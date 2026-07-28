package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/config"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/dispatch"
	"github.com/loop-engineering/runner/internal/health"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if handled, err := probe(os.Args[1:]); handled {
		if err != nil {
			slog.Error("runner probe failed", "error", err)
			os.Exit(1)
		}
		fmt.Printf("{\"status\":%q}\n", os.Args[1])
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("runner stopped", "error", err)
		os.Exit(1)
	}
}

// osEnvironmentResolver resolves a task packet's EnvironmentRefs from the
// runner process's own environment. Operators configure the runner host or
// container with the secret material (e.g. GITHUB_TOKEN) under the same
// name declared in LOOP_RUNNER_ALLOWED_ENVIRONMENT; the SecretRef field is
// carried for audit purposes only and does not select a backend here.
type osEnvironmentResolver struct{}

func (osEnvironmentResolver) Resolve(_ context.Context, references []taskpacket.EnvironmentRef) (map[string]string, error) {
	resolved := make(map[string]string, len(references))
	for _, reference := range references {
		value, ok := os.LookupEnv(reference.Name)
		if !ok || value == "" {
			return nil, fmt.Errorf("environment reference %q is not configured on this runner", reference.Name)
		}
		resolved[reference.Name] = value
	}
	return resolved, nil
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
	dispatcher := &dispatch.Dispatcher{
		Workspaces:         repository.Manager{DataDirectory: settings.DataDir},
		RevisionInspector:  repository.Manager{DataDirectory: settings.DataDir},
		Delivery:           repository.Manager{DataDirectory: settings.DataDir},
		Backend:            agentBackend(settings),
		Environment:        osEnvironmentResolver{},
		AllowedEnvironment: settings.AllowedEnvironment,
		MinimumFreeBytes:   settings.MinimumFreeBytes,
		DiskPath:           settings.DataDir,
		AvailableBytes:     health.AvailableBytes,
		Retention:          retentionPolicy(settings.WorkspaceRetention),
		Projects:           projects,
	}
	loop, err := dispatch.NewControlLoop(
		client,
		dispatcher,
		time.Now,
		60*time.Second,
		15*time.Second,
		settings.RedactionPrefixes,
		settings.EventOutboxPath(),
	)
	if err != nil {
		return fmt.Errorf("create runner dispatch loop: %w", err)
	}
	loop.ReconnectMin = settings.ReconnectMin
	loop.ReconnectMax = settings.ReconnectMax
	dispatcher.EmitLog = loop.Reporter.EmitLog
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
					slog.Warn("runner configuration reload rejected", "runner_id", identity.RunnerID, "error", err)
					continue
				}
				streamSettings.Reload(next)
				slog.Info("runner control settings reloaded", "runner_id", identity.RunnerID)
			}
		}
	}()

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var dispatchErr atomic.Value
	go func() {
		if err := loop.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("runner dispatch loop stopped", "runner_id", identity.RunnerID, "error", err)
			dispatchErr.Store(err)
			cancelRun()
		}
	}()
	slog.Info("runner initialized", "runner_id", identity.RunnerID, "orchestrator_endpoint", settings.OrchestratorEndpoint)
	supervisorErr := control.StreamSupervisor{
		Client:            client,
		Labels:            settings.Labels,
		HeartbeatInterval: settings.HeartbeatInterval,
		ReconnectMin:      settings.ReconnectMin,
		ReconnectMax:      settings.ReconnectMax,
		Busy:              loop.Busy,
		OnConnected:       loop.FlushEvents,
		OnHeartbeat:       loop.Reconcile,
		Settings:          streamSettings.Get,
	}.Run(runCtx)
	if stored := dispatchErr.Load(); stored != nil {
		return fmt.Errorf("runner dispatch loop stopped: %w", stored.(error))
	}
	return supervisorErr
}
