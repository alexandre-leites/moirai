package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/config"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/dispatch"
	"github.com/loop-engineering/runner/internal/execution"
	"github.com/loop-engineering/runner/internal/health"
	"github.com/loop-engineering/runner/internal/metrics"
	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
	"github.com/loop-engineering/runner/internal/toolchain"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if handled, err := toolchainCommand(os.Args[1:], toolchain.DefaultManifestPath, os.Stdout); handled {
		if err != nil {
			slog.Error("runner toolchain declaration is not usable", "error", err)
			os.Exit(1)
		}
		return
	}
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
// name declared in LOOP_RUNNER_ALLOWED_ENVIRONMENT, either as a plain
// variable or as a "<NAME>_FILE" path to a mounted Compose secret.
type osEnvironmentResolver struct{}

func (osEnvironmentResolver) Resolve(_ context.Context, _ dispatch.SecretScope, references []taskpacket.EnvironmentRef) (map[string]string, error) {
	resolved := make(map[string]string, len(references))
	for _, reference := range references {
		value, err := config.SecretValue(os.LookupEnv, reference.Name)
		if err != nil {
			return nil, fmt.Errorf("environment reference %q cannot be read on this runner: %w", reference.Name, err)
		}
		if value == "" {
			return nil, fmt.Errorf("environment reference %q is not configured on this runner", reference.Name)
		}
		resolved[reference.Name] = value
	}
	return resolved, nil
}

// controlPlaneResolver asks the orchestrator for each reference first and falls
// back to the runner's own environment when it declines.
//
// That order is what makes a runner "dumb": a project that configures its own
// credential needs nothing provisioned here, and one that does not keeps
// working exactly as it did. The fallback is not silent -- the control plane
// only declines for reasons that mean "there is nothing for you here" (no such
// credential, a stale lease, no TLS on this deployment). A credential that
// exists but could not be served is an error, and fails the execution.
type controlPlaneResolver struct {
	secrets  *control.SecretResolver
	fallback osEnvironmentResolver
}

// DiscardJobKeys satisfies dispatch.SecretDiscarder, so the dispatcher removes
// anything this resolver wrote to disk once the job is over.
func (r controlPlaneResolver) DiscardJobKeys(jobID string) error {
	return r.secrets.DiscardJobKeys(jobID)
}

func (r controlPlaneResolver) Resolve(ctx context.Context, scope dispatch.SecretScope, references []taskpacket.EnvironmentRef) (map[string]string, error) {
	resolved := make(map[string]string, len(references))
	var remaining []taskpacket.EnvironmentRef
	for _, reference := range references {
		value, path, err := r.secrets.Resolve(ctx, scope.JobID, scope.LeaseGeneration, reference.Name)
		switch {
		case errors.Is(err, control.ErrNoControlPlaneSecret):
			remaining = append(remaining, reference)
			continue
		case err != nil:
			return nil, err
		}
		if path != "" {
			// A key is delivered as a path: ssh cannot read a variable, and the
			// material itself must not become one that every child process
			// inherits. The reference's name carries the path instead.
			resolved[reference.Name] = path
			continue
		}
		resolved[reference.Name] = value
	}
	if len(remaining) == 0 {
		return resolved, nil
	}
	fromEnvironment, err := r.fallback.Resolve(ctx, scope, remaining)
	if err != nil {
		return nil, err
	}
	for name, value := range fromEnvironment {
		resolved[name] = value
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

func pipelineRunner(settings config.Config) pipeline.Runner {
	if settings.DockerEnabled {
		return pipeline.DockerRunner{Executor: execution.DockerExecutor{Image: settings.AgentDockerImage, CPULimit: settings.DockerCPULimit, MemoryLimit: settings.DockerMemoryLimit, Network: settings.DockerNetwork, StopTimeout: settings.DockerStopTimeout}}
	}
	return pipeline.LocalRunner{}
}

func executionEnvironment(settings config.Config) func(string) (agents.Backend, pipeline.Runner, error) {
	return func(image string) (agents.Backend, pipeline.Runner, error) {
		if _, err := exec.LookPath("docker"); err != nil {
			return nil, nil, fmt.Errorf("execution image %q cannot run on this runner: Docker executable is unavailable", image)
		}
		executor := execution.DockerExecutor{Image: image, CPULimit: settings.DockerCPULimit, MemoryLimit: settings.DockerMemoryLimit, Network: settings.DockerNetwork, StopTimeout: settings.DockerStopTimeout}
		arguments := append([]string(nil), settings.AgentArguments...)
		switch settings.AgentBackend {
		case "cli":
			arguments = append([]string{settings.AgentBinary}, arguments...)
		case "opencode":
			arguments = append([]string{"opencode", "run"}, arguments...)
		}
		return agents.DockerCLIBackend{Image: image, Arguments: arguments, Executor: executor}, pipeline.DockerRunner{Executor: executor}, nil
	}
}

func agentBackend(settings config.Config) agents.Backend {
	if settings.AgentBackend == "cli" {
		return agents.CLIBackend{NameValue: "cli", Binary: settings.AgentBinary, Arguments: settings.AgentArguments}
	}
	if settings.AgentBackend == "docker" {
		return agents.DockerCLIBackend{Image: settings.AgentDockerImage, Arguments: settings.AgentArguments, Executor: execution.DockerExecutor{CPULimit: settings.DockerCPULimit, MemoryLimit: settings.DockerMemoryLimit, Network: settings.DockerNetwork, StopTimeout: settings.DockerStopTimeout}}
	}
	return agents.OpenCodeBackend{Arguments: settings.AgentArguments}
}

// toolchainCommand implements `runner toolchain [--verify]`.
//
// It exists so the image's declaration of what it offers the agent has one
// implementation with three readers: the image build, which runs --verify and
// fails rather than shipping a manifest that lies; CI, which runs the same
// check against the built image, closing the gap left by an assertion that only
// covered the programs the *runner* shells out to; and anyone -- an operator or
// an agent inside the image -- who wants to read the declaration back.
//
// It is answered before configuration is loaded, because the question is about
// the image rather than about a deployment, and the answer has to be available
// during the build, when nothing is configured yet.
func toolchainCommand(arguments []string, manifestPath string, out io.Writer) (bool, error) {
	if len(arguments) == 0 || arguments[0] != "toolchain" {
		return false, nil
	}
	verify := false
	switch {
	case len(arguments) == 1:
	case len(arguments) == 2 && arguments[1] == "--verify":
		verify = true
	default:
		return true, errors.New("usage: runner toolchain [--verify]")
	}
	manifest, err := toolchain.Load(manifestPath)
	if err != nil {
		return true, err
	}
	if verify {
		// Verified against the PATH the agent is given rather than the one this
		// process happens to hold. A tool the runner can reach and the agent
		// cannot is exactly the kind of absence this is here to prevent.
		agentPath := execution.MinimalEnvironmentMap(nil, os.TempDir())["PATH"]
		if err := manifest.Verify(func(name string) bool { return toolchain.LookupIn(agentPath, name) }); err != nil {
			return true, err
		}
	}
	_, err = fmt.Fprint(out, manifest.Declaration())
	return true, err
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

func retentionPolicy(settings config.Config) dispatch.RetentionPolicy {
	policy := dispatch.RetentionPolicy{
		MaxAge:        settings.RetentionMaxAge,
		MaxWorkspaces: settings.RetentionMaxCount,
		Directory:     settings.DataDir,
	}
	for _, value := range settings.WorkspaceRetention {
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
	metricsServer := metrics.New(settings.MetricsBind)
	metricsServer.Start()
	recovered, err := agents.ReconcileManifests(settings.DataDir)
	if err != nil {
		return fmt.Errorf("reconcile runner executions: %w", err)
	}
	service, connection, err := control.DialWithHeaders(ctx, settings.OrchestratorEndpoint, control.TLSOptions{
		Enabled:        settings.TLS,
		CAFile:         settings.TLSCAFile,
		ClientCertFile: settings.TLSClientCertFile,
		ClientKeyFile:  settings.TLSClientKeyFile,
		ServerName:     settings.TLSServerName,
	}, control.HeaderOptions{Headers: settings.OrchestratorHeaders, File: settings.OrchestratorHeadersFile})
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
		settings.Capacity,
	)
	if err != nil {
		return fmt.Errorf("bootstrap runner identity: %w", err)
	}
	client, err := control.NewClientFromGRPC(service, identity)
	if err != nil {
		return fmt.Errorf("create runner control client: %w", err)
	}
	// Key material from a previous life is removed before any job can start: a
	// crash mid-job leaves a private key on the tmpfs, and the next execution
	// must not inherit one it was never granted.
	secrets := control.NewSecretResolver(service, identity, settings.KeyDir)
	// Reported at startup rather than discovered midway through the first job
	// that needs a key -- but not fatal. A deployment that never uses
	// file-delivered secrets (no TLS, or no project with an SSH key) works
	// perfectly well without this directory, and refusing to start would take
	// those runners down for a feature they do not use.
	if err := secrets.EnsureKeyDirectory(); err != nil {
		slog.Warn("job key directory is unusable; a project with an SSH key will fail on this runner", "error", err)
	}
	if err := secrets.DiscardJobKeys(""); err != nil {
		return fmt.Errorf("clear leftover job keys: %w", err)
	}
	projects := dispatch.NewProjectConcurrencyGuard()
	repositories := repository.Manager{DataDirectory: settings.DataDir, CleanupAttempts: settings.CleanupAttempts, CleanupRetryDelay: settings.CleanupRetryDelay, LockPollInterval: settings.RepositoryLockPoll, GitCommitterName: settings.GitCommitterName, GitCommitterEmail: settings.GitCommitterEmail}
	dispatcher := &dispatch.Dispatcher{
		Workspaces:           repositories,
		RevisionInspector:    repositories,
		Delivery:             repositories,
		Backend:              agentBackend(settings),
		ExecutionEnvironment: executionEnvironment(settings),
		Pipeline:             pipelineRunner(settings),
		Environment:          controlPlaneResolver{secrets: secrets},
		AllowedEnvironment:   settings.AllowedEnvironment,
		MinimumFreeBytes:     settings.MinimumFreeBytes,
		DiskPath:             settings.DataDir,
		AvailableBytes:       health.AvailableBytes,
		Retention:            retentionPolicy(settings),
		PushWorkInProgress:   settings.PushWorkInProgress,
		MaxContinuations:     settings.MaxContinuations,
		Projects:             projects,
		Active:               dispatch.NewActiveWorkspaces(),
	}
	// Retained workspaces age while the runner is idle, so the age bound is also
	// applied once at startup rather than only before the next execution.
	if err := dispatcher.SweepRetainedWorkspaces(ctx); err != nil {
		slog.Warn("could not fully sweep retained workspaces at startup", "error", err)
	}
	loop, err := dispatch.NewControlLoopWithEventLimits(
		client,
		dispatcher,
		time.Now,
		settings.LeaseDuration,
		settings.LeaseRenewalLead,
		settings.RedactionPrefixes,
		settings.EventOutboxPath(),
		settings.Capacity,
		settings.EventBufferSize,
		settings.EventPayloadBytes,
		settings.LogChunkBytes,
	)
	if err != nil {
		return fmt.Errorf("create runner dispatch loop: %w", err)
	}
	for _, execution := range recovered {
		if err := loop.ReportRecoveredFailure(execution.JobID, execution.LeaseGeneration, execution.ExecutionID); err != nil {
			return fmt.Errorf("report recovered execution: %w", err)
		}
	}
	loop.ReconnectMin = settings.ReconnectMin
	loop.ReconnectMax = settings.ReconnectMax
	loop.ExpiryInterval = settings.HeartbeatInterval
	loop.OfferTimeout = settings.OfferTimeout
	// Point the loop and its event reporter at the recorder the metrics server
	// actually serves. Both default to the process-wide recorder, which is the
	// same object today, but stating it here is what keeps them pointed at the
	// served registry if the server is ever built with a recorder of its own.
	loop.UseMetrics(metricsServer.Recorder())
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
	supervisorErr := controlStreamSupervisor(client, loop, settings, streamSettings, metricsServer.MarkHeartbeat).Run(runCtx)
	if stored := dispatchErr.Load(); stored != nil {
		return fmt.Errorf("runner dispatch loop stopped: %w", stored.(error))
	}
	if ctx.Err() != nil {
		loop.Drain()
		shutdown, cancel := context.WithTimeout(context.Background(), settings.TerminationGrace)
		err := loop.WaitForIdle(shutdown)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			loop.CancelAll()
		}
	}
	return supervisorErr
}

// controlStreamSupervisor wires the control loop to the stream supervisor. It
// is a named function rather than a literal inside run() so the wiring is
// testable — in particular OnConnected, which is what reports the runner's
// drain state on every stream it establishes and so is the whole of the runner
// side of issue #148.
func controlStreamSupervisor(client control.StreamSupervisorClient, loop *dispatch.ControlLoop, settings config.Config, streamSettings *reloadableStreamSettings, markHeartbeat func()) control.StreamSupervisor {
	return control.StreamSupervisor{
		Client:            client,
		Labels:            settings.Labels,
		HeartbeatInterval: settings.HeartbeatInterval,
		ReconnectMin:      settings.ReconnectMin,
		ReconnectMax:      settings.ReconnectMax,
		Busy:              loop.Busy,
		OnConnected:       loop.Resume,
		OnHeartbeat: func() error {
			if err := loop.Reconcile(); err != nil {
				return err
			}
			markHeartbeat()
			return nil
		},
		Settings: streamSettings.Get,
	}
}
