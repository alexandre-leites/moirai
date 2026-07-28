package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/config"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/http/handlers"
	"github.com/loop-engineering/api/internal/orchestrator"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	runtimeConfig, err := config.FromEnvironment()
	if err != nil {
		logger.Error("runtime configuration error", "error", err)
		os.Exit(1)
	}
	dialContext, cancelDial := context.WithTimeout(context.Background(), 10*time.Second)
	client, err := orchestrator.Dial(dialContext, runtimeConfig.OrchestratorEndpoint)
	cancelDial()
	if err != nil {
		logger.Error("orchestrator connection error", "error", err)
		os.Exit(1)
	}
	defer client.Close()

	cfg := apiserver.DefaultConfig()
	cfg.BindAddress = runtimeConfig.BindAddress
	cfg.MaxRequestBodyBytes = runtimeConfig.MaxBodyBytes
	cfg.OrchestratorHealthy = client.Healthy

	srv, err := apiserver.New(cfg, logger)
	if err != nil {
		logger.Error("server configuration error", "error", err)
		os.Exit(1)
	}
	loginLimiter := auth.NewRateLimiter(time.Minute, 10, auth.WithTrustedProxies(runtimeConfig.TrustedProxies))
	mutationLimiter := auth.NewRateLimiter(time.Minute, 60, auth.WithTrustedProxies(runtimeConfig.TrustedProxies))

	handlers.NewAuthHandlers(client, runtimeConfig.CookieSecure, loginLimiter).RegisterRoutes(srv.Mux())
	handlers.NewProjectHandlers(client, mutationLimiter).RegisterRoutes(srv.Mux())
	handlers.NewRunnerTokenHandlers(client, mutationLimiter).RegisterRoutes(srv.Mux())
	handlers.NewRunnerHandlers(client, mutationLimiter).RegisterRoutes(srv.Mux())
	handlers.NewQueueHandlers(client).RegisterRoutes(srv.Mux())
	handlers.NewEventHandlers(client).RegisterRoutes(srv.Mux())
	handlers.NewWorkflowHandlers(client, mutationLimiter).RegisterRoutes(srv.Mux())

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("api server shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}
