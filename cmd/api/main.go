package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github/nallanos/fire2/internal/app"
	"github/nallanos/fire2/internal/logging"
	"github/nallanos/fire2/internal/packages/orchestrator"
)

func main() {
	logging.SetupLogger("fire2-api")

	cfg := app.ConfigFromEnv()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("pgxpool.New failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		slog.Error("rivermigrate.New failed", "err", err)
		os.Exit(1)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		slog.Error("river migrate up failed", "err", err)
		os.Exit(1)
	}

	riverClient, err := app.NewRiverClient(pool, cfg, &orchestrator.StrongRetryPolicy{})
	if err != nil {
		slog.Error("river.NewClient failed", "err", err)
		os.Exit(1)
	}

	if err := riverClient.Start(ctx); err != nil {
		slog.Error("riverClient.Start failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	}()

	a := app.New(cfg, pool, riverClient)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           a.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "port", cfg.Port)
		errCh <- srv.ListenAndServe()
	}()

	grpcErrCh := make(chan error, 1)
	go func() {
		grpcAddr := cfg.OrchestratorGRPCBindHost + ":" + cfg.OrchestratorGRPCPort
		slog.Info("orchestrator gRPC listening", "address", grpcAddr)
		grpcErrCh <- orchestrator.ServeEventGRPC(grpcAddr, app.NewEventServer(pool, riverClient))
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		slog.Error("server error", "err", err)
	case err := <-grpcErrCh:
		slog.Error("grpc server error", "err", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
