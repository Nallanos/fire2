package orchestrator

import (
	"context"
	"fmt"
	"log"

	"github.com/riverqueue/river"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// CleanupSandboxWorker handles cleanup_sandbox jobs enqueued when CreateSandboxWorker
// exhausts all attempts. It removes the container from the assigned worker (best-effort)
// and marks the sandbox as failed with worker_id cleared.
type CleanupSandboxWorker struct {
	river.WorkerDefaults[CleanupSandboxArgs]
	sandboxRepo sandboxpkg.Repository
	workerRepo  workerpkg.Repository
}

func NewCleanupSandboxWorker(sandboxRepo sandboxpkg.Repository, workerRepo workerpkg.Repository) *CleanupSandboxWorker {
	return &CleanupSandboxWorker{sandboxRepo: sandboxRepo, workerRepo: workerRepo}
}

func (w *CleanupSandboxWorker) Work(ctx context.Context, job *river.Job[CleanupSandboxArgs]) error {
	sandboxID := job.Args.SandboxID

	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("cleanup: get sandbox %s: %w", sandboxID, err)
	}

	// Idempotent: if already fully cleaned up, do nothing.
	if sbx.Status == sandboxpkg.StatusFailed || sbx.Status == sandboxpkg.StatusCleanedUp {
		return nil
	}

	// Best-effort: ask the assigned worker to remove the container.
	if sbx.WorkerID != nil {
		w.removeFromWorker(ctx, sbx)
	}

	// Clear worker_id unconditionally.
	if err := w.sandboxRepo.ClearWorker(ctx, sandboxID); err != nil {
		log.Printf("cleanup: clear worker_id failed for sandbox=%s: %v", sandboxID, err)
	}

	// Transition to failed regardless of which non-terminal state we're in.
	_, _, err = w.sandboxRepo.UpdateStatus(ctx, sandboxID, sandboxpkg.StatusFailed,
		sandboxpkg.StatusCleanupPending, sandboxpkg.StatusPending, sandboxpkg.StatusScheduling,
		sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning)
	if err != nil {
		return fmt.Errorf("cleanup: set failed for sandbox=%s: %w", sandboxID, err)
	}

	return nil
}

func (w *CleanupSandboxWorker) removeFromWorker(ctx context.Context, sbx sandboxpkg.Sandbox) {
	worker, err := w.workerRepo.Get(ctx, *sbx.WorkerID)
	if err != nil {
		log.Printf("cleanup: get worker %s for sandbox %s: %v — skipping remote remove", *sbx.WorkerID, sbx.ID, err)
		return
	}

	addr := normalizeWorkerAddress(worker.Address, int32(worker.Port))
	client, err := NewClient(ctx, addr)
	if err != nil {
		log.Printf("cleanup: connect worker %s for sandbox %s: %v — skipping remote remove", *sbx.WorkerID, sbx.ID, err)
		return
	}
	defer client.Close()

	_, err = client.RemoveSandbox(ctx, &workerv1.RemoveSandboxRequest{ContainerId: sbx.ID})
	if err != nil {
		log.Printf("cleanup: RemoveSandbox for sandbox=%s on worker=%s: %v — continuing", sbx.ID, *sbx.WorkerID, err)
	}
}
