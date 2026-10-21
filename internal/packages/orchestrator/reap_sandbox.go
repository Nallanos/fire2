package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

// ReapSandboxQueue is the River queue the sandbox reaper runs on.
const ReapSandboxQueue = "reap_sandbox"

// ReapSandboxArgs are the arguments for the sandbox-reaper periodic job. It
// carries no data — each run sweeps every sandbox row.
type ReapSandboxArgs struct{}

func (ReapSandboxArgs) Kind() string { return "reap_sandbox" }

// ReapSandboxWorker claims sandboxes that must be torn down and enqueues their
// cleanup. Two causes:
//   - TTL: created_at + ttl (seconds) has passed → cleanup ends in stopped.
//   - Liveness: the hosting worker has not listed the sandbox in a heartbeat
//     inventory for longer than livenessTimeout → cleanup ends in failed.
//
// Claiming (status → cleanup_pending) and enqueueing happen in one transaction,
// and the claim skips locked rows, so concurrent reapers never double-enqueue
// and a crash between the two steps cannot strand a claimed sandbox.
type ReapSandboxWorker struct {
	river.WorkerDefaults[ReapSandboxArgs]
	pool            *pgxpool.Pool
	sandboxRepo     sandboxpkg.Repository
	livenessTimeout time.Duration
}

func NewReapSandboxWorker(sandboxRepo sandboxpkg.Repository, pool *pgxpool.Pool, livenessTimeout time.Duration) *ReapSandboxWorker {
	return &ReapSandboxWorker{sandboxRepo: sandboxRepo, pool: pool, livenessTimeout: livenessTimeout}
}

func (w *ReapSandboxWorker) Work(ctx context.Context, job *river.Job[ReapSandboxArgs]) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reap sandbox: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	repo := w.sandboxRepo.WithTx(tx)

	expired, err := repo.ClaimTTLExpired(ctx)
	if err != nil {
		return fmt.Errorf("reap sandbox: claim ttl-expired: %w", err)
	}
	dead, err := repo.ClaimLivenessExpired(ctx, w.livenessTimeout)
	if err != nil {
		return fmt.Errorf("reap sandbox: claim liveness-expired: %w", err)
	}

	if len(expired)+len(dead) == 0 {
		return nil
	}

	params := make([]river.InsertManyParams, 0, len(expired)+len(dead))
	for _, id := range expired {
		params = append(params, cleanupParams(id, sandboxpkg.StatusStopped))
	}
	for _, id := range dead {
		params = append(params, cleanupParams(id, sandboxpkg.StatusFailed))
	}

	if _, err := river.ClientFromContext[pgx.Tx](ctx).InsertManyTx(ctx, tx, params); err != nil {
		return fmt.Errorf("reap sandbox: enqueue cleanup: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("reap sandbox: commit: %w", err)
	}

	slog.Info("reap sandbox: claimed expired sandboxes", "ttl_expired", len(expired), "liveness_expired", len(dead))
	return nil
}

func cleanupParams(sandboxID string, target sandboxpkg.Status) river.InsertManyParams {
	return river.InsertManyParams{
		Args:       CleanupSandboxArgs{SandboxID: sandboxID, TargetStatus: string(target)},
		InsertOpts: &river.InsertOpts{Queue: "cleanup"},
	}
}
