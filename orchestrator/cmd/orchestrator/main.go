package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator"
	"github.com/loop-engineering/orchestrator/internal/config"
	"github.com/loop-engineering/orchestrator/internal/metrics"
	"github.com/loop-engineering/orchestrator/internal/migrate"
	"github.com/loop-engineering/orchestrator/internal/server"
	"google.golang.org/grpc"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheckExitCode())
	}
	if err := run(); err != nil {
		slog.Error("orchestrator stopped", "error", err)
		os.Exit(1)
	}
}

// healthcheckExitCode is the `orchestrator healthcheck` subcommand Docker
// invokes: a separate process from the running server, so it has no way to
// read that process's in-memory loop-liveness state directly. What it *can*
// do is fetch /readyz on the metrics listener, which the running server
// updates from that same state on every request (see metrics.readyHandler) --
// an HTTP round trip through loopback rather than a shared-memory read, but
// the only channel a subprocess actually has into the other process.
//
// Metrics can be explicitly disabled (LOOP_METRICS_BIND=""), in which case no
// such channel exists at all, and this falls back to the old bare TCP dial: it
// still catches "the process is gone" but not "a loop stalled", which is the
// best a healthcheck can do with no readiness endpoint to ask.
func healthcheckExitCode() int {
	address, ok := readinessAddress()
	if !ok {
		return livenessExitCode()
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + address + "/readyz")
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// livenessExitCode is the bare-dial check the healthcheck subcommand used
// before this file tracked loop liveness at all, kept as the fallback for a
// deployment that has explicitly turned the metrics listener off.
func livenessExitCode() int {
	connection, err := net.DialTimeout("tcp", healthcheckAddress(), time.Second)
	if err == nil {
		err = connection.Close()
	}
	if err != nil {
		return 1
	}
	return 0
}

// healthcheckAddress dials the port the server was actually told to bind, so
// that overriding LOOP_GRPC_BIND does not leave the container permanently
// unhealthy. The host is always loopback: the probe runs inside the container.
func healthcheckAddress() string {
	bind := os.Getenv("LOOP_GRPC_BIND")
	if bind == "" {
		bind = config.DefaultGRPCBind
	}
	_, port, err := net.SplitHostPort(bind)
	if err != nil || port == "" {
		_, port, _ = net.SplitHostPort(config.DefaultGRPCBind)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// readinessAddress is the loopback address /readyz is served on, derived the
// same way healthcheckAddress derives the gRPC one. The bool is false when
// LOOP_METRICS_BIND was explicitly set empty: metrics, and therefore
// readiness, are deliberately off, and there is no port here to ask.
func readinessAddress() (string, bool) {
	bind := config.DefaultMetricsBind
	if configured, set := os.LookupEnv("LOOP_METRICS_BIND"); set {
		bind = strings.TrimSpace(configured)
	}
	if bind == "" {
		return "", false
	}
	_, port, err := net.SplitHostPort(bind)
	if err != nil || port == "" {
		_, port, _ = net.SplitHostPort(config.DefaultMetricsBind)
	}
	return net.JoinHostPort("127.0.0.1", port), true
}

// observed reports each pass of a reconciliation loop to the metrics recorder
// and passes the error through untouched, so the loop's own logging and retry
// behaviour is unchanged. Wrapping here rather than inside the loop bodies
// keeps the server package free of a metrics dependency it would otherwise
// carry into every test.
func observed(recorder *metrics.Recorder, loop string, fn func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		err := fn(ctx)
		// A pass cut short by shutdown is not a failed pass, and `every` does
		// not log it as one either. Counting it would make every clean restart
		// look like a reconciliation failure.
		if err != nil && ctx.Err() != nil {
			return err
		}
		recorder.RecordLoopRun(loop, err)
		return err
	}
}

// every runs fn immediately and then on each tick until ctx is cancelled,
// logging failures rather than exiting: these are background reconciliation
// loops, and one bad pass is not a reason to stop making the next one.
func every(ctx context.Context, interval time.Duration, name string, fn func(context.Context) error) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				slog.Error(name+" failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// maxScheduledPerTick bounds how many jobs a single scheduler tick may place
// via drainSchedule before yielding back to the ticker. Without a bound, a
// queue that keeps ScheduleOnce returning true (the fifty-eligible-issues
// case #290 was filed about) would let one tick run indefinitely, starving
// every other loop `every` drives on this same process and its shared
// database pool -- the workflow observer, the recovery sweep, issue sync --
// for as long as the queue stayed non-empty. 500 is chosen well above any
// realistic single-tick fleet size while still bounding the worst case: at
// roughly one round trip to Postgres per claim, a few hundred claims cost low
// tens of milliseconds, comfortably inside the 1-second tick this loop
// shares with everything else. A queue deeper than that drains over
// successive ticks instead of one, which is a far better failure mode than a
// scheduler tick that never yields.
const maxScheduledPerTick = 500

// drainSchedule repeatedly calls scheduleOnce -- ScheduleOnce in production --
// until it reports nothing left to place (false, nil) or maxScheduledPerTick
// placements have happened in this tick, instead of the fixed one-job-per-tick
// rate that discarding ScheduleOnce's bool return produced (#290): a queue
// with fifty eligible issues and ten idle runners used to fill at one job per
// second rather than draining in a single pass.
//
// Runner capacity (#272) is enforced inside ClaimSchedulableIssue's own WHERE
// clause, re-evaluated fresh on every call within its own committed
// transaction, so calling scheduleOnce in a tight loop does not schedule past
// what a runner's registered capacity allows -- the loop just reaches the
// same "nothing left to claim" (false) sooner once every idle runner is full.
func drainSchedule(ctx context.Context, scheduleOnce func(context.Context) (bool, error)) error {
	for i := 0; i < maxScheduledPerTick; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		scheduled, err := scheduleOnce(ctx)
		if err != nil {
			return err
		}
		if !scheduled {
			return nil
		}
	}
	return nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrate.Apply(ctx, cfg.DatabaseURL, orchestrator.Migrations); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.GRPCBind)
	if err != nil {
		return err
	}
	defer listener.Close()
	transport, err := cfg.ServerCredentials()
	if err != nil {
		return err
	}
	var options []grpc.ServerOption
	if transport != nil {
		options = append(options, grpc.Creds(transport))
	}
	slog.Info("serving gRPC", "bind", cfg.GRPCBind, "tls", transport != nil, "mtls", cfg.TLSClientCAFile != "")
	grpcServer := grpc.NewServer(options...)
	service, err := server.New(pool, os.Getenv("MOIRAI_BUILD_VERSION"))
	if err != nil {
		return err
	}
	if err := service.Bootstrap(ctx); err != nil {
		return err
	}
	// Settle the database state the previous process left behind before serving:
	// runners still recorded as online, and leases nobody is renewing. Both are
	// two statements against Postgres and both need to be true before runners
	// start reconnecting.
	//
	// Resuming interrupted deliveries is deliberately *not* done here. It shells
	// out to `gh` once per stranded workflow, and the gRPC listener is already
	// open by this point, so a slow or hung GitHub would leave connections
	// sitting in the accept queue for as long as this took. The metrics listener
	// /readyz answers from is not up yet either, so the healthcheck fails closed
	// (connection refused) for this window rather than reporting ready early.
	// The periodic sweep picks up any stranded delivery within 30 seconds.
	if err := service.ReconcileDatabaseOnce(ctx); err != nil {
		slog.Error("startup recovery failed", "error", err)
	}
	// Queue depth, active workflow counts and the fleet-wide runner heartbeat
	// age are derived from this process's database and exist in no other
	// service's reach, so this listener is the only place they are published.
	// Binding fails loudly rather than being logged and dropped: an
	// observability endpoint that silently never listens is indistinguishable
	// from a healthy one that nothing is scraping.
	metricsServer := metrics.New(cfg.MetricsBind, service)
	if err := metricsServer.Start(); err != nil {
		return err
	}
	if metricsServer.Enabled() {
		// The bound address, not the configured one: they differ when the
		// configured port was 0, and the address a scraper needs is the one
		// that was actually taken.
		slog.Info("serving metrics", "bind", metricsServer.Addr(), "path", "/metrics")
		// Deferred, so /metrics stays scrapeable for the whole of the gRPC
		// graceful drain below. The pool is closed by an earlier defer, so it
		// outlives this one; a scrape still running when Shutdown's own
		// deadline expires can still lose its connection under it, which the
		// collector reports as a failed scrape rather than a crash.
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := metricsServer.Shutdown(shutdown); err != nil {
				slog.Warn("metrics server shutdown failed", "error", err)
			}
		}()
	}
	recorder := metricsServer.Recorder()
	// Issue sync is the one loop whose interval is operator-configurable
	// (LOOP_ISSUE_SYNC_INTERVAL); every other loop's is fixed in code and the
	// recorder already knows it (see metrics.defaultLoopIntervals). This has to
	// happen before the loop below starts recording into it.
	recorder.SetLoopInterval(metrics.LoopIssueSync, cfg.IssueSyncInterval)
	// So GetSchedulerMetrics can report the same loop liveness the healthcheck's
	// /readyz endpoint does -- one recorder, read two ways, never two answers.
	service.SetLoopRecorder(recorder)
	controlv1.RegisterControlPlaneServer(grpcServer, service)
	runnerv1.RegisterRunnerControlServer(grpcServer, service)
	every(ctx, time.Second, "scheduler tick", observed(recorder, metrics.LoopScheduler, func(ctx context.Context) error {
		return drainSchedule(ctx, service.ScheduleOnce)
	}))
	every(ctx, 15*time.Second, "workflow observer", observed(recorder, metrics.LoopWorkflowObserver, service.ObserveWorkflows))
	every(ctx, 30*time.Second, "recovery sweep", observed(recorder, metrics.LoopRecoverySweep, service.RecoverOnce))
	// Issues are otherwise only discovered when an operator opens the console and
	// presses "Sync now", which leaves an unattended deployment idle no matter how
	// much work its projects have queued.
	every(ctx, cfg.IssueSyncInterval, "issue sync", observed(recorder, metrics.LoopIssueSync, service.SyncProjects))
	go func() {
		<-ctx.Done()
		// Must run before GracefulStop: GracefulStop blocks until every
		// in-flight RPC returns, and Connect/StreamEvents only return early
		// because they observe this signal. Calling it after GracefulStop
		// has already started blocking this same goroutine would deadlock.
		service.Shutdown()
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
