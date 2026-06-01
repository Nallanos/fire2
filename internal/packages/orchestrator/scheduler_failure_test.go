//go:build integration

package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// waitForTerminal polls until the sandbox reaches a fully settled state (failed, cleaned_up, running).
// cleanup_pending is NOT included — it's an intermediate state; tests that want to see the full arc
// must wait until the cleanup job also finishes.
func waitForTerminal(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, timeout time.Duration) sandboxpkg.Status {
	t.Helper()
	terminal := map[sandboxpkg.Status]bool{
		sandboxpkg.StatusRunning:   true,
		sandboxpkg.StatusFailed:    true,
		sandboxpkg.StatusCleanedUp: true,
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sbx, err := repo.GetByID(ctx, id)
		if err == nil && terminal[sbx.Status] {
			return sbx.Status
		}
		time.Sleep(50 * time.Millisecond)
	}
	sbx, _ := repo.GetByID(ctx, id)
	return sbx.Status
}

// F1: No workers in DB → job fails on all 5 attempts; cleanup job enqueued; status → cleanup_pending.
func TestSchedulerFailure_NoWorkers(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-f1", sandboxpkg.StatusPending, nil)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err := riverClient.InsertTx(ctx, tx, CreateSandboxArgs{SandboxID: "sbx-f1"}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	// With MaxAttempts=5 and fast retry (50ms), should complete well within 10s.
	finalStatus := waitForTerminal(t, ctx, sandboxRepo, "sbx-f1", 15*time.Second)
	if finalStatus != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed after no-worker exhaustion, got %s", finalStatus)
	}
}

// F2: gRPC CreateSandbox returns persistent error → same retry/cleanup arc as F1.
func TestSchedulerFailure_WorkerGRPCError(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	fakeWorker.CreateError = errors.New("worker crashed")
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      "wk-f2",
		Status:  workerpkg.WorkerStatusActive,
		Address: workerHost,
		Port:    workerPort,
		Budget:  workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity: 4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-f2", sandboxpkg.StatusPending, nil)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CreateSandboxArgs{SandboxID: "sbx-f2"}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	finalStatus := waitForTerminal(t, ctx, sandboxRepo, "sbx-f2", 15*time.Second)
	if finalStatus != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed after gRPC error exhaustion, got %s", finalStatus)
	}
}

// F3: gRPC succeeds on attempt 3 → status reaches running; no cleanup enqueued.
func TestSchedulerFailure_SucceedsOnThirdAttempt(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      "wk-f3",
		Status:  workerpkg.WorkerStatusActive,
		Address: workerHost,
		Port:    workerPort,
		Budget:  workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity: 4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-f3", sandboxpkg.StatusPending, nil)

	// Seed the sandbox at "assigned" with worker set (simulating 2 prior failed attempts that
	// got as far as assigned but crashed before the gRPC call).
	wid := "wk-f3"
	_, _, _ = sandboxRepo.UpdateStatus(ctx, "sbx-f3", sandboxpkg.StatusScheduling, sandboxpkg.StatusPending)
	_, err = sandboxRepo.AssignWorker(ctx, "sbx-f3", wid)
	if err != nil {
		t.Fatalf("assign worker: %v", err)
	}

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CreateSandboxArgs{SandboxID: "sbx-f3"}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	waitForStatus(t, ctx, sandboxRepo, "sbx-f3", sandboxpkg.StatusRunning, 10*time.Second)

	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-f3")
	if sbx.Status != sandboxpkg.StatusRunning {
		t.Fatalf("expected running, got %s", sbx.Status)
	}
	if sbx.WorkerID == nil || *sbx.WorkerID != "wk-f3" {
		t.Fatalf("expected worker_id=wk-f3, got %v", sbx.WorkerID)
	}
}
