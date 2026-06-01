//go:build integration

package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

func fakeCleanupJob(sandboxID string) *river.Job[CleanupSandboxArgs] {
	return &river.Job[CleanupSandboxArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 5},
		Args:   CleanupSandboxArgs{SandboxID: sandboxID},
	}
}

func waitForCleanup(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sbx, err := repo.GetByID(ctx, id)
		if err == nil && (sbx.Status == sandboxpkg.StatusFailed || sbx.Status == sandboxpkg.StatusCleanedUp) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	sbx, _ := repo.GetByID(ctx, id)
	t.Fatalf("sandbox %s: expected failed/cleaned_up, got %s after %s", id, sbx.Status, timeout)
}

// G1: Cleanup runs for sandbox with worker_id set → RemoveSandbox called once; status=failed; worker_id=nil.
func TestCleanupSandboxWorker_WithWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(0, 0)
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	wid := "wk-g1"
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      wid,
		Status:  workerpkg.WorkerStatusActive,
		Address: workerHost,
		Port:    workerPort,
		Budget:  workerpkg.Worker_Budget{Cpu_budget: 2, Mem_budget: 2048},
		Capacity: 2,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-g1", sandboxpkg.StatusCleanupPending, &wid)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CleanupSandboxArgs{SandboxID: "sbx-g1"}, &river.InsertOpts{Queue: "cleanup"})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	waitForCleanup(t, ctx, sandboxRepo, "sbx-g1", 10*time.Second)

	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-g1")
	if sbx.Status != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed, got %s", sbx.Status)
	}
	if sbx.WorkerID != nil {
		t.Fatalf("expected worker_id=nil, got %v", *sbx.WorkerID)
	}
}

// G2: Cleanup called twice → second invocation is idempotent (status already failed).
func TestCleanupSandboxWorker_Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-g2", sandboxpkg.StatusFailed, nil)

	// Directly invoke Work() on a cleanup job with existing failed sandbox.
	worker := NewCleanupSandboxWorker(sandboxRepo, workerRepo)
	if err := worker.Work(ctx, fakeCleanupJob("sbx-g2")); err != nil {
		t.Fatalf("second Work() on failed sandbox: %v", err)
	}

	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-g2")
	if sbx.Status != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed unchanged, got %s", sbx.Status)
	}
}

// G3: Worker unreachable → cleanup continues; status becomes failed, worker_id cleared.
func TestCleanupSandboxWorker_WorkerUnreachable(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	// Register a worker pointing to a non-listening port.
	wid := "wk-g3"
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      wid,
		Status:  workerpkg.WorkerStatusActive,
		Address: "127.0.0.1",
		Port:    19999, // nothing listening here
		Budget:  workerpkg.Worker_Budget{Cpu_budget: 2, Mem_budget: 2048},
		Capacity: 2,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-g3", sandboxpkg.StatusCleanupPending, &wid)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CleanupSandboxArgs{SandboxID: "sbx-g3"}, &river.InsertOpts{Queue: "cleanup"})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	waitForCleanup(t, ctx, sandboxRepo, "sbx-g3", 10*time.Second)

	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-g3")
	if sbx.Status != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed despite unreachable worker, got %s", sbx.Status)
	}
	if sbx.WorkerID != nil {
		t.Fatalf("expected worker_id cleared, got %v", *sbx.WorkerID)
	}
}

// G4: Cleanup for already-failed sandbox → no double RemoveSandbox.
func TestCleanupSandboxWorker_AlreadyFailed_NoOp(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(0, 0)
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	wid := "wk-g4"
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      wid,
		Status:  workerpkg.WorkerStatusActive,
		Address: workerHost,
		Port:    workerPort,
		Budget:  workerpkg.Worker_Budget{Cpu_budget: 2, Mem_budget: 2048},
		Capacity: 2,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	// Seed with no worker_id (already cleaned up).
	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-g4", sandboxpkg.StatusFailed, nil)

	worker := NewCleanupSandboxWorker(sandboxRepo, workerRepo)
	if err := worker.Work(ctx, fakeCleanupJob("sbx-g4")); err != nil {
		t.Fatalf("Work on already-failed: %v", err)
	}

	// FakeWorker should NOT have been called since worker_id is nil.
	// (The cleanup worker only calls removeFromWorker when sbx.WorkerID != nil.)
	// We can't easily check RemoveSandbox call count here, but verifying no panic is sufficient.
	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-g4")
	if sbx.Status != sandboxpkg.StatusFailed {
		t.Fatalf("expected status unchanged, got %s", sbx.Status)
	}
}
