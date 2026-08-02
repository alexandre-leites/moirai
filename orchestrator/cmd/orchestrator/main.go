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
	"github.com/loop-engineering/orchestrator/internal/config"
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
	_, port, err := net.SplitHostPort(os.Getenv("LOOP_GRPC_BIND"))
	if err != nil || port == "" {
		port = "50051"
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// syncInterval is how often issues are re-read from the issue tracker. It is
// configurable because the useful cadence depends on the tracker's rate limits
// and on how quickly a team expects a newly labelled issue to be picked up.
func syncInterval() time.Duration {
	if value := os.Getenv("LOOP_ISSUE_SYNC_INTERVAL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
		}
		slog.Warn("ignoring invalid LOOP_ISSUE_SYNC_INTERVAL", "value", value)
	}
	return 2 * time.Minute
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
	if err := migrate.Apply(ctx, cfg.DatabaseURL, os.DirFS(".")); err != nil {
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
	// Before serving anything, settle what the previous process left behind:
	// runners recorded as online that hold no stream here, leases nobody is
	// renewing, and deliveries interrupted between the runner's completion and
	// the pull request. Each of those holds a project lock, and a held lock
	// stops that project scheduling anything at all.
	if err := service.RecoverOnce(ctx); err != nil {
		slog.Error("startup recovery failed", "error", err)
	}
	controlv1.RegisterControlPlaneServer(grpcServer, service)
	runnerv1.RegisterRunnerControlServer(grpcServer, service)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if _, err := service.ScheduleOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("scheduler tick failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			if err := service.ObserveWorkflows(ctx); err != nil && ctx.Err() == nil {
				slog.Error("workflow observer failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if err := service.RecoverOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Error("recovery sweep failed", "error", err)
			}
		}
	}()
	// Issues are otherwise only discovered when an operator opens the console
	// and presses "Sync now", which leaves an unattended deployment idle no
	// matter how much work its projects have queued.
	go func() {
		ticker := time.NewTicker(syncInterval())
		defer ticker.Stop()
		for {
			if err := service.SyncProjects(ctx); err != nil && ctx.Err() == nil {
				slog.Error("issue sync failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
