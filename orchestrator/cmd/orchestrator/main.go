package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
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
		connection, err := net.DialTimeout("tcp", healthcheckAddress(), time.Second)
		if err == nil {
			err = connection.Close()
		}
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("orchestrator stopped", "error", err)
		os.Exit(1)
	}
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
	// out to `gh` once per stranded workflow, and the listener is already open
	// by this point, so a slow or hung GitHub would leave connections sitting in
	// the accept queue while the healthcheck — a bare TCP dial — reported the
	// container ready. The periodic sweep picks those up within 30 seconds.
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
	controlv1.RegisterControlPlaneServer(grpcServer, service)
	runnerv1.RegisterRunnerControlServer(grpcServer, service)
	every(ctx, time.Second, "scheduler tick", func(ctx context.Context) error {
		_, err := service.ScheduleOnce(ctx)
		return err
	})
	every(ctx, 15*time.Second, "workflow observer", service.ObserveWorkflows)
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
