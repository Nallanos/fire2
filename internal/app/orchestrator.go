package app

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github/nallanos/fire2/internal/packages/orchestrator"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// NewRiverClient builds the orchestrator's River client: the five workers, the
// three queues and the two periodic reapers, whose intervals and timeouts come
// from cfg. retry is the backoff of the default queue's jobs. The caller runs
// the River migrations beforehand and starts the client.
//
// cmd/api and the divergence test harness both build their client here, so the
// harness exercises exactly the wiring that runs in production.
func NewRiverClient(pool *pgxpool.Pool, cfg Config, retry river.ClientRetryPolicy) (*river.Client[pgx.Tx], error) {
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	workers := river.NewWorkers()
	river.AddWorker(workers, orchestrator.NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
	river.AddWorker(workers, orchestrator.NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	river.AddWorker(workers, orchestrator.NewReapWorkersWorker(workerRepo, cfg.HeartbeatTimeout))
	river.AddWorker(workers, orchestrator.NewReapSandboxWorker(sandboxRepo, pool, cfg.SandboxLivenessTimeout))
	river.AddWorker(workers, orchestrator.NewRemoveOrphanWorker(workerRepo))

	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault:            {MaxWorkers: 10},
			"cleanup":                     {MaxWorkers: 5},
			orchestrator.ReapSandboxQueue: {MaxWorkers: 1},
		},
		Workers:     workers,
		MaxAttempts: 5,
		RetryPolicy: retry,
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
}

// NewEventServer builds the gRPC service workers register, heartbeat and report
// their sandbox events to. It reconciles heartbeat inventories in a transaction
// (WithTransactions) and enqueues orphan removals and lost-sandbox cleanups
// through riverClient (WithRiverClient).
func NewEventServer(pool *pgxpool.Pool, riverClient *river.Client[pgx.Tx]) *orchestrator.EventGRPCServer {
	return orchestrator.NewEventGRPCServer(
		sandboxpkg.NewPostgresRepository(pool),
		orchestrator.NewEventRepository(pool),
		workerpkg.NewPostgresRepository(pool),
		orchestrator.WithTransactions(pool),
		orchestrator.WithRiverClient(riverClient),
	)
}
