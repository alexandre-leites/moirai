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
		connection, err := net.DialTimeout("tcp", "127.0.0.1:50051", time.Second)
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
	grpcServer := grpc.NewServer()
	service, err := server.New(pool, os.Getenv("MOIRAI_BUILD_VERSION"))
	if err != nil {
		return err
	}
	if err := service.Bootstrap(ctx); err != nil {
		return err
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
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
