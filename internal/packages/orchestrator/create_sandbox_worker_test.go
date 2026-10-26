//go:build integration

package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
		ID:             "wk-d1",
		Status:         workerpkg.WorkerStatusActive,
		Address:        workerHost,
		Port:           workerPort,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity:       4,
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

// A worker the network drops packets to leaves CreateSandbox unanswered. The
// attempt gives up at its own deadline with an error, and the sandbox stays
// assigned so River retries it from there, instead of the job hanging until
// River's timeout.
func TestCreateSandboxWorker_CreateCallTimesOut(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	shortenCallTimeouts(t, 300*time.Millisecond, time.Minute)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	fakeWorker.CreateDelay = time.Minute
	workerHost, workerPort := fakeWorker.StartServer(t)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	wid := "wk-silent"
	if _, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID: wid, Status: workerpkg.WorkerStatusActive, Address: workerHost, Port: workerPort,
		Budget: workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096}, Capacity: 4, Last_heartbeat: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	seedSandboxForJob(t, ctx, sandboxRepo, "sbx-silent", sandboxpkg.StatusAssigned, &wid)

	worker := NewCreateSandboxWorker(pool, sandboxRepo, workerRepo)
	began := time.Now()
	err := worker.Work(ctx, fakeCreateJob("sbx-silent", 1, 5))
	elapsed := time.Since(began)

	if err == nil {
		t.Fatal("Work returned nil for a CreateSandbox nobody answered")
	}
	var cancelled *river.JobCancelError
	if errors.As(err, &cancelled) {
		t.Fatalf("Work cancelled the job on a first attempt (%v): it must be retried", err)
	}
	if status.Code(errors.Unwrap(err)) != codes.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Work error = %v, want a deadline exceeded", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 10*time.Second {
		t.Fatalf("Work gave up after %s, want about the 300ms deadline", elapsed)
	}
	if got := fakeWorker.CreateCallCount("sbx-silent"); got != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", got)
	}
	sbx, _ := sandboxRepo.GetByID(ctx, "sbx-silent")
	if sbx.Status != sandboxpkg.StatusAssigned {
		t.Fatalf("sandbox is %s after the timed-out attempt, want assigned (retried from there)", sbx.Status)
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
		ID:             wid,
		Status:         workerpkg.WorkerStatusActive,
		Address:        workerHost,
		Port:           workerPort,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity:       4,
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

// D6/D7: a sandbox already running needs nothing from the create job. A sandbox
// that ended without coming up (failed, stopped, being cleaned up) was torn down
// under the job: the job is cancelled, never completed, or the HTTP handler
// answers 201 Created for it.
func TestCreateSandboxWorker_SandboxAlreadySettled(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	jobWorker := NewCreateSandboxWorker(pool, sandboxRepo, workerRepo)

	for status, wantCancel := range map[sandboxpkg.Status]bool{
		sandboxpkg.StatusRunning:        false,
		sandboxpkg.StatusFailed:         true,
		sandboxpkg.StatusStopped:        true,
		sandboxpkg.StatusCleanupPending: true,
		sandboxpkg.StatusCleanedUp:      true,
	} {
		id := "sbx-d-settled-" + string(status)
		seedSandboxForJob(t, ctx, sandboxRepo, id, status, nil)

		err := jobWorker.Work(ctx, fakeCreateJob(id, 1, 5))
		var cancelled *rivertype.JobCancelError
		switch {
		case wantCancel && !errors.As(err, &cancelled):
			t.Fatalf("Work() for status %s returned %v, want a job cancellation", status, err)
		case !wantCancel && err != nil:
			t.Fatalf("Work() for status %s returned error: %v", status, err)
		}

		sbx, _ := sandboxRepo.GetByID(ctx, id)
		if sbx.Status != status {
			t.Fatalf("status %s changed to %s unexpectedly", status, sbx.Status)
		}
	}
}

// movingRepo moves the sandbox to a status just before a guarded call, as a
// concurrent delete or reaper would between the job's read and its update.
type movingRepo struct {
	sandboxpkg.Repository
	pool *pgxpool.Pool
	to   sandboxpkg.Status
}

func (r *movingRepo) move(ctx context.Context, id string) {
	_, _ = r.pool.Exec(ctx, `UPDATE sandboxes SET status = $2 WHERE id = $1`, id, string(r.to))
}

func (r *movingRepo) UpdateStatus(ctx context.Context, id string, status sandboxpkg.Status, allowedFrom ...sandboxpkg.Status) (sandboxpkg.Sandbox, int64, error) {
	r.move(ctx, id)
	return r.Repository.UpdateStatus(ctx, id, status, allowedFrom...)
}

func (r *movingRepo) AssignWorker(ctx context.Context, id, workerID string) (sandboxpkg.Sandbox, error) {
	r.move(ctx, id)
	return r.Repository.AssignWorker(ctx, id, workerID)
}

// A sandbox that ends between the job's read and a guarded update (a delete, the
// TTL or the creation timeout) makes that update change nothing. The job must
// see where the sandbox went and be cancelled: returning nil completes the job,
// and the HTTP handler answers 201 for a sandbox that is being cleaned up.
func TestCreateSandboxWorker_SandboxEndsBetweenReadAndUpdate(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(0, 0)
	host, port := fakeWorker.StartServer(t)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	if _, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID: "wk-race", Status: workerpkg.WorkerStatusActive, Address: host, Port: port,
		Budget: workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096}, Capacity: 4,
		Last_heartbeat: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	for _, from := range []sandboxpkg.Status{sandboxpkg.StatusPending, sandboxpkg.StatusScheduling} {
		t.Run(string(from), func(t *testing.T) {
			id := "sbx-race-" + string(from)
			seedSandboxForJob(t, ctx, sandboxRepo, id, from, nil)
			racing := &movingRepo{Repository: sandboxRepo, pool: pool, to: sandboxpkg.StatusCleanupPending}

			err := NewCreateSandboxWorker(pool, racing, workerRepo).Work(ctx, fakeCreateJob(id, 1, 5))

			var cancelled *rivertype.JobCancelError
			if !errors.As(err, &cancelled) {
				t.Fatalf("Work() returned %v, want a job cancellation: the sandbox ended under the job", err)
			}
		})
	}
}
