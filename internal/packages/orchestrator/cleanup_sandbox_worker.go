package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// CleanupSandboxWorker handles cleanup_sandbox jobs: it removes the sandbox from
// its assigned worker, then moves it to the job's terminal status (failed by
// default) and clears worker_id.
//
// The terminal transition only happens once the worker confirms the removal.
// Until then the sandbox stays cleanup_pending and keeps its worker_id, and the
// job returns an error so River retries. On the final attempt the link to the
// worker is dropped and the sandbox ends up failed, whatever the requested
// target, since its removal was never confirmed.
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
	isFinalAttempt := job.Attempt >= job.MaxAttempts

	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("cleanup: get sandbox %s: %w", sandboxID, err)
	}

	// Idempotent: if already fully cleaned up, do nothing.
	switch sbx.Status {
	case sandboxpkg.StatusFailed, sandboxpkg.StatusStopped, sandboxpkg.StatusCleanedUp:
		return nil
	}

	target := sandboxpkg.StatusFailed
	if job.Args.TargetStatus != "" {
		target = sandboxpkg.Status(job.Args.TargetStatus)
	}

	if sbx.WorkerID != nil {
		if err := removeSandboxOnWorker(ctx, w.workerRepo, *sbx.WorkerID, sandboxID); err != nil {
			if !isFinalAttempt {
				return fmt.Errorf("cleanup: sandbox=%s: %w", sandboxID, err)
			}
			slog.Error("cleanup: giving up on remote removal", "sandbox_id", sandboxID, "attempts", job.Attempt, "err", err)
			target = sandboxpkg.StatusFailed
		}

		if err := w.sandboxRepo.ClearWorker(ctx, sandboxID); err != nil {
			return fmt.Errorf("cleanup: clear worker_id for sandbox=%s: %w", sandboxID, err)
		}
	}

	// Transition to the target terminal status regardless of which
	// non-terminal state we're in.
	_, _, err = w.sandboxRepo.UpdateStatus(ctx, sandboxID, target,
		sandboxpkg.StatusCleanupPending, sandboxpkg.StatusPending, sandboxpkg.StatusScheduling,
		sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning)
	if err != nil {
		return fmt.Errorf("cleanup: set %s for sandbox=%s: %w", target, sandboxID, err)
	}

	return nil
}
