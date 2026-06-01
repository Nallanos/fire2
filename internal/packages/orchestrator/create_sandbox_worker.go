package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// CreateSandboxArgs are the arguments for the create-sandbox river job.
// Only the sandbox ID is stored; all other fields are read from the DB row
// so job args never drift from the source of truth.
type CreateSandboxArgs struct {
	SandboxID string `json:"sandbox_id"`
}

func (CreateSandboxArgs) Kind() string { return "create_sandbox" }

// CleanupSandboxArgs are the arguments for the cleanup river job.
type CleanupSandboxArgs struct {
	SandboxID string `json:"sandbox_id"`
}

func (CleanupSandboxArgs) Kind() string { return "cleanup_sandbox" }

// CreateSandboxWorker processes create_sandbox jobs.
// It drives the sandbox through the state machine one step per attempt;
// because status is written only after a step succeeds, retries are safe.
type CreateSandboxWorker struct {
	river.WorkerDefaults[CreateSandboxArgs]
	pool        *pgxpool.Pool
	sandboxRepo sandboxpkg.Repository
	workerRepo  workerpkg.Repository
	scheduler   *Scheduler
}

func NewCreateSandboxWorker(
	pool *pgxpool.Pool,
	sandboxRepo sandboxpkg.Repository,
	workerRepo workerpkg.Repository,
) *CreateSandboxWorker {
	return &CreateSandboxWorker{
		pool:        pool,
		sandboxRepo: sandboxRepo,
		workerRepo:  workerRepo,
		scheduler:   NewScheduler(),
	}
}

func (w *CreateSandboxWorker) Work(ctx context.Context, job *river.Job[CreateSandboxArgs]) error {
	sandboxID := job.Args.SandboxID
	isFinalAttempt := job.Attempt >= job.MaxAttempts

	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("get sandbox %s: %w", sandboxID, err)
	}

	switch sbx.Status {
	case sandboxpkg.StatusPending:
		return w.stepScheduling(ctx, sandboxID, isFinalAttempt, job)
	case sandboxpkg.StatusScheduling:
		return w.stepAssign(ctx, sbx, isFinalAttempt, job)
	case sandboxpkg.StatusAssigned:
		return w.stepStartContainer(ctx, sbx, isFinalAttempt, job)
	case sandboxpkg.StatusStarting:
		return w.stepAcknowledge(ctx, sandboxID, isFinalAttempt, job)
	case sandboxpkg.StatusRunning,
		sandboxpkg.StatusStopped,
		sandboxpkg.StatusFailed,
		sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusCleanedUp:
		return nil // already terminal or running — idempotent no-op
	default:
		return fmt.Errorf("unexpected sandbox status %q for sandbox %s", sbx.Status, sandboxID)
	}
}

// stepScheduling transitions pending → scheduling.
func (w *CreateSandboxWorker) stepScheduling(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	_, n, err := w.sandboxRepo.UpdateStatus(ctx, sandboxID, sandboxpkg.StatusScheduling, sandboxpkg.StatusPending)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, fmt.Errorf("set scheduling: %w", err))
	}
	if n == 0 {
		// Race: status was already advanced by a concurrent attempt — re-read and continue.
		return nil
	}
	// Re-enter to run the scheduling step in the same Work() call.
	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, err)
	}
	return w.stepAssign(ctx, sbx, isFinal, job)
}

// stepAssign picks a worker and transitions scheduling → assigned.
func (w *CreateSandboxWorker) stepAssign(ctx context.Context, sbx sandboxpkg.Sandbox, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	if sbx.WorkerID != nil {
		// Already assigned — skip straight to container start.
		return w.stepStartContainer(ctx, sbx, isFinal, job)
	}

	workers, err := w.workerRepo.List(ctx)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("list workers: %w", err))
	}

	candidates := buildCandidates(ctx, workers)
	chosen, err := w.scheduler.ChooseLeastUsedWorker(candidates)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("choose worker: %w", err))
	}

	assigned, err := w.sandboxRepo.AssignWorker(ctx, sbx.ID, chosen.Worker.ID)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("assign worker: %w", err))
	}
	if assigned.WorkerID == nil {
		// Guard rejected the update — status advanced concurrently, re-read.
		return nil
	}

	return w.stepStartContainer(ctx, assigned, isFinal, job)
}

// stepStartContainer calls the worker gRPC and transitions assigned → starting.
func (w *CreateSandboxWorker) stepStartContainer(ctx context.Context, sbx sandboxpkg.Sandbox, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	if sbx.WorkerID == nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, errors.New("sandbox has no worker_id at assigned step"))
	}

	// Resolve full address from worker row.
	worker, err := w.workerRepo.Get(ctx, *sbx.WorkerID)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("get worker %s: %w", *sbx.WorkerID, err))
	}
	address := normalizeWorkerAddress(worker.Address, int32(worker.Port))

	client, err := NewClient(ctx, address)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("connect worker %s: %w", *sbx.WorkerID, err))
	}
	defer client.Close()

	_, grpcErr := client.CreateSandbox(ctx, &workerv1.CreateSandboxRequest{
		Id:         sbx.ID,
		Runtime:    sbx.Runtime,
		Image:      sbx.Image,
		Port:       sbx.Port,
		Ttl:        sbx.TTL,
		PreviewUrl: sbx.PreviewURL,
	})
	if grpcErr != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("worker CreateSandbox: %w", grpcErr))
	}

	_, n, err := w.sandboxRepo.UpdateStatus(ctx, sbx.ID, sandboxpkg.StatusStarting, sandboxpkg.StatusAssigned)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("set starting: %w", err))
	}
	if n == 0 {
		log.Printf("create_sandbox: status guard rejected starting update for sandbox=%s; may have been advanced by event handler", sbx.ID)
	}

	return w.stepAcknowledge(ctx, sbx.ID, isFinal, job)
}

// stepAcknowledge transitions starting → running (the event handler may have done this already).
func (w *CreateSandboxWorker) stepAcknowledge(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, err)
	}
	if sbx.Status == sandboxpkg.StatusRunning {
		return nil // event handler already flipped it
	}
	if sbx.Status == sandboxpkg.StatusFailed {
		return nil // container died — event handler flipped it; cleanup will be scheduled elsewhere
	}

	_, _, err = w.sandboxRepo.UpdateStatus(ctx, sandboxID, sandboxpkg.StatusRunning,
		sandboxpkg.StatusStarting)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, fmt.Errorf("set running: %w", err))
	}
	return nil
}

// maybeCleanup: on the final attempt, enqueue a cleanup job and mark the sandbox;
// otherwise just return the error so river retries.
func (w *CreateSandboxWorker) maybeCleanup(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs], origErr error) error {
	if !isFinal {
		return origErr
	}

	log.Printf("create_sandbox: final attempt failed for sandbox=%s err=%v; enqueueing cleanup", sandboxID, origErr)

	tx, txErr := w.pool.Begin(ctx)
	if txErr != nil {
		log.Printf("create_sandbox: could not begin cleanup tx for sandbox=%s: %v", sandboxID, txErr)
		return origErr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, updateErr := w.sandboxRepo.WithTx(tx).UpdateStatus(ctx, sandboxID, sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusPending, sandboxpkg.StatusScheduling, sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning)
	if updateErr != nil {
		log.Printf("create_sandbox: could not set cleanup_pending for sandbox=%s: %v", sandboxID, updateErr)
		return origErr
	}

	riverClient := river.ClientFromContext[pgx.Tx](ctx)
	_, insertErr := riverClient.InsertTx(ctx, tx, CleanupSandboxArgs{SandboxID: sandboxID}, &river.InsertOpts{Queue: "cleanup"})
	if insertErr != nil {
		log.Printf("create_sandbox: could not insert cleanup job for sandbox=%s: %v", sandboxID, insertErr)
		return origErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		log.Printf("create_sandbox: cleanup tx commit failed for sandbox=%s: %v", sandboxID, commitErr)
		return origErr
	}

	return river.JobCancel(fmt.Errorf("max attempts exhausted for sandbox=%s: %w", sandboxID, origErr))
}

// buildCandidates contacts each worker via gRPC to fetch current load info.
func buildCandidates(ctx context.Context, workers []workerpkg.Worker) []WorkerCandidate {
	gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	candidates := make([]WorkerCandidate, 0, len(workers))
	for _, w := range workers {
		addr := normalizeWorkerAddress(w.Address, int32(w.Port))
		c, err := NewClient(gctx, addr)
		if err != nil {
			continue
		}
		info, err := c.GetWorkerInfo(gctx, &workerv1.GetWorkerInfoRequest{})
		_ = c.Close()
		if err != nil {
			continue
		}

		// Wrap in the db.Worker-shaped struct the scheduler expects.
		candidates = append(candidates, WorkerCandidate{
			Worker: w,
			Info:   info,
		})
	}
	return candidates
}
