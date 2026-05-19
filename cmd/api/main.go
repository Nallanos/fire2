package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github/nallanos/fire2/internal/app"
	dbpkg "github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/orchestrator"
)

func main() {
	cfg := app.ConfigFromEnv()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Wrap pool as *sql.DB so existing sqlc-generated queries keep working unchanged.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	// Apply River schema migrations at startup.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		log.Fatalf("rivermigrate.New: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		log.Fatalf("river migrate up: %v", err)
	}

	queries := dbpkg.New(sqlDB)

	workers := river.NewWorkers()
	river.AddWorker(workers, orchestrator.NewCreateSandboxWorker(queries))

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:      map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers:     workers,
		MaxAttempts: 5,
		RetryPolicy: &orchestrator.StrongRetryPolicy{},
	})
	if err != nil {
		log.Fatalf("river.NewClient: %v", err)
	}

	if err := riverClient.Start(ctx); err != nil {
		log.Fatalf("riverClient.Start: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	}()

	a := app.New(cfg, sqlDB, riverClient)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           a.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api listening on :%s", cfg.Port)
		errCh <- srv.ListenAndServe()
	}()

	grpcErrCh := make(chan error, 1)
	go func() {
		grpcAddr := ":" + cfg.OrchestratorGRPCPort
		log.Printf("orchestrator gRPC listening on %s", grpcAddr)
		grpcErrCh <- orchestrator.ServeEventGRPC(grpcAddr, orchestrator.NewEventGRPCServer(queries))
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("shutdown signal: %s", sig)
	case err := <-errCh:
		log.Printf("server error: %v", err)
	case err := <-grpcErrCh:
		log.Printf("grpc server error: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
