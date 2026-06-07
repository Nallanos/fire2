package orchestrator

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/riverqueue/river"

	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// ReapWorkersArgs are the arguments for the worker-reaper periodic job. It
// carries no data — the job sweeps every worker row on each run.
type ReapWorkersArgs struct{}

func (ReapWorkersArgs) Kind() string { return "reap_workers" }

// ReapWorkersWorker demotes workers whose heartbeat has gone stale to
// inactive, so the scheduler stops routing to dead workers. A worker that
// recovers re-registers itself as active on its next heartbeat, so this is
// self-healing.
type ReapWorkersWorker struct {
	river.WorkerDefaults[ReapWorkersArgs]
	workerRepo workerpkg.Repository
	timeout    time.Duration
}

func NewReapWorkersWorker(workerRepo workerpkg.Repository, timeout time.Duration) *ReapWorkersWorker {
	return &ReapWorkersWorker{workerRepo: workerRepo, timeout: timeout}
}

func (w *ReapWorkersWorker) Work(ctx context.Context, job *river.Job[ReapWorkersArgs]) error {
	n, err := w.workerRepo.MarkStaleInactive(ctx, w.timeout)
	if err != nil {
		return fmt.Errorf("reap workers: %w", err)
	}
	if n > 0 {
		log.Printf("reaper: marked %d stale worker(s) inactive", n)
	}
	return nil
}
