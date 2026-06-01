//go:build integration

package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

func fakeCreateJob(sandboxID string, attempt, maxAttempts int) *river.Job[CreateSandboxArgs] {
	return &river.Job[CreateSandboxArgs]{
		JobRow: &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts},
		Args:   CreateSandboxArgs{SandboxID: sandboxID},
	}
}

// setupWorkerJob creates a worker row and a sandbox row at the given status, then inserts
// a create_sandbox job and returns the job ID so the test can wait for completion.
func setupWorkerForJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sandboxID string, initStatus sandboxpkg.Status) (workerHost string, workerPort int) {
	t.Helper()
	wk := testutil.NewFakeWorkerServer(10, 10) // low usage
	h, p := wk.StartServer(t)
	return h, p
}

func seedSandboxForJob(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, status sandboxpkg.Status, workerID *string) {
	t.Helper()
	_, err := repo.Create(ctx, sandboxpkg.Sandbox{
		ID:        id,
		Runtime:   "node",
		Status:    status,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		WorkerID:  workerID,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
}

func waitForStatus(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, want sandboxpkg.Status, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sbx, err := repo.GetByID(ctx, id)
		if err == nil && sbx.Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	sbx, _ := repo.GetByID(ctx, id)
	t.Fatalf("sandbox %s: expected status %s, got %s after %s", id, want, sbx.Status, timeout)
}

// D1: Job starting from pending advances through to assigned; worker CreateSandbox called once
// even after two Work() invocations (second is idempotent because status is already running).
func TestCreateSandboxWorker_PendingToRunning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	// Register the worker.
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      "wk-d1",
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

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-d1", sandboxpkg.StatusPending, nil)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CreateSandboxArgs{SandboxID: "sbx-d1"}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	waitForStatus(t, ctx, sandboxRepo, "sbx-d1", sandboxpkg.StatusRunning, 10*time.Second)

	// Verify worker was called exactly once.
	if fakeWorker.CreateCallCount("sbx-d1") != 1 {
		t.Fatalf("expected 1 CreateSandbox call, got %d", fakeWorker.CreateCallCount("sbx-d1"))
	}

	// D5: Running sandbox — second Work() call (if job retried) is a no-op.
	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-d1")
	if sbx.Status != sandboxpkg.StatusRunning {
		t.Fatalf("expected running, got %s", sbx.Status)
	}
}

// D4: sandbox already at starting → single Work() advances to running without gRPC.
func TestCreateSandboxWorker_StartingToRunning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(0, 0)
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	wid := "wk-d4"
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      wid,
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

	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-d4", sandboxpkg.StatusStarting, &wid)

	riverClient := testutil.SetupRiverClient(t, ctx, pool, func(workers *river.Workers) {
		river.AddWorker(workers, NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
		river.AddWorker(workers, NewCleanupSandboxWorker(sandboxRepo, workerRepo))
	})

	tx, _ := pool.Begin(ctx)
	_, err = riverClient.InsertTx(ctx, tx, CreateSandboxArgs{SandboxID: "sbx-d4"}, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("InsertTx: %v", err)
	}
	_ = tx.Commit(ctx)

	waitForStatus(t, ctx, sandboxRepo, "sbx-d4", sandboxpkg.StatusRunning, 5*time.Second)

	// No gRPC CreateSandbox call at the starting→running step.
	if fakeWorker.CreateCallCount("sbx-d4") != 0 {
		t.Fatalf("expected 0 CreateSandbox calls from starting state, got %d", fakeWorker.CreateCallCount("sbx-d4"))
	}
}

// D6/D7: Terminal and already-running statuses → Work() is a no-op.
func TestCreateSandboxWorker_TerminalStatus_NoOp(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	jobWorker := NewCreateSandboxWorker(pool, sandboxRepo, workerRepo)

	for _, status := range []sandboxpkg.Status{
		sandboxpkg.StatusFailed,
		sandboxpkg.StatusRunning,
		sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusCleanedUp,
	} {
		id := "sbx-d-terminal-" + string(status)
		seedSandboxForJob(t, ctx, sandboxRepo, id, status, nil)

		if err := jobWorker.Work(ctx, fakeCreateJob(id, 1, 5)); err != nil {
			t.Fatalf("Work() for status %s returned error: %v", status, err)
		}

		sbx, _ := sandboxRepo.GetByID(ctx, id)
		if sbx.Status != status {
			t.Fatalf("status %s changed to %s unexpectedly", status, sbx.Status)
		}
	}
}
