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
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github/nallanos/fire2/internal/app"
	"github/nallanos/fire2/internal/packages/orchestrator"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

func main() {
	app.SetupLogger("fire2-api")

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

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	eventRepo := orchestrator.NewEventRepository(pool)

	workers := river.NewWorkers()
	river.AddWorker(workers, orchestrator.NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
	river.AddWorker(workers, orchestrator.NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	river.AddWorker(workers, orchestrator.NewReapWorkersWorker(workerRepo, cfg.HeartbeatTimeout))
	river.AddWorker(workers, orchestrator.NewReapSandboxWorker(sandboxRepo, pool, cfg.SandboxLivenessTimeout))
	river.AddWorker(workers, orchestrator.NewRemoveOrphanWorker(workerRepo))

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault:            {MaxWorkers: 10},
			"cleanup":                     {MaxWorkers: 5},
			orchestrator.ReapSandboxQueue: {MaxWorkers: 1},
		},
		Workers:     workers,
		MaxAttempts: 5,
		RetryPolicy: &orchestrator.StrongRetryPolicy{},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(cfg.ReaperInterval),
				func() (river.JobArgs, *river.InsertOpts) { return orchestrator.ReapWorkersArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(cfg.SandboxReaperInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return orchestrator.ReapSandboxArgs{}, &river.InsertOpts{Queue: orchestrator.ReapSandboxQueue}
				},
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
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
		grpcErrCh <- orchestrator.ServeEventGRPC(grpcAddr,
			orchestrator.NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo,
				orchestrator.WithTransactions(pool),
				orchestrator.WithRiverClient(riverClient)))
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
