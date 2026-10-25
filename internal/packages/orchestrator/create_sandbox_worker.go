package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// Bounds on the orchestrator's calls to a worker. A worker the network drops
// packets to (not one that refuses the connection) would otherwise hold the job
// until River's own timeout, and keep a HTTP caller waiting.
const (
	// candidatesTimeout bounds all the GetWorkerInfo calls of one scheduling
	// step (buildCandidates).
	candidatesTimeout = 5 * time.Second
	// defaultCreateSandboxCallTimeout bounds one CreateSandbox call. The worker
	// bounds the boot to 30s itself (sandboxmgr bootTimeout), then answers.
	defaultCreateSandboxCallTimeout = 45 * time.Second
	// defaultRemoveSandboxCallTimeout bounds one removal, from the database
	// read of the worker to the worker's answer. Removing includes stopping the
	// VM (3s of grace, then a kill) and the device-mapper EBUSY retries, up to 5s.
	defaultRemoveSandboxCallTimeout = 15 * time.Second

	// JobTimeout is what a River job gets (river.Config.JobTimeout): the slowest
	// attempt of the create job, a scheduling step then a CreateSandbox call,
	// plus room for its database round trips. Left to River's default (1m) the
	// room would be 10s, one slow query from a job cancelled mid-call.
	JobTimeout = candidatesTimeout + defaultCreateSandboxCallTimeout + 30*time.Second
)

// Variables so a test does not wait the real delays.
var (
	createSandboxCallTimeout = defaultCreateSandboxCallTimeout
	removeSandboxCallTimeout = defaultRemoveSandboxCallTimeout
)

// CleanupSandboxArgs are the arguments for the cleanup river job.
type CleanupSandboxArgs struct {
	SandboxID string `json:"sandbox_id"`
	// TargetStatus is the terminal status to set once cleanup completes.
	// Empty defaults to StatusFailed, matching the original caller
	// (maybeCleanup, after exhausted create-job retries). A user-initiated
	// delete sets this to StatusStopped instead — same teardown, different
	// terminal meaning.
	TargetStatus string `json:"target_status,omitempty"`
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
		return w.stepStartSandbox(ctx, sbx, isFinalAttempt, job)
	case sandboxpkg.StatusStarting:
		return w.stepAcknowledge(ctx, sandboxID, isFinalAttempt, job)
	case sandboxpkg.StatusRunning,
		sandboxpkg.StatusStopped,
		sandboxpkg.StatusFailed,
		sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusCleanedUp:
		_, err := settled(sbx)
		return err
	default:
		return fmt.Errorf("unexpected sandbox status %q for sandbox %s", sbx.Status, sandboxID)
	}
}

// settled reports whether the create job has nothing left to do for sbx: it
// is running (nil), or it ended without coming up (a cancellation).
//
// A sandbox can end under its job: liveness, the TTL, a delete, a dead VM.
// Such a job must be cancelled, not completed. A completed job tells the HTTP
// handler the sandbox was created, and it answers 201 for a sandbox that is
// gone. Cancelling also stops River from retrying a job that cannot succeed.
func settled(sbx sandboxpkg.Sandbox) (bool, error) {
	switch sbx.Status {
	case sandboxpkg.StatusRunning:
		return true, nil
	case sandboxpkg.StatusStopped, sandboxpkg.StatusFailed, sandboxpkg.StatusCleanupPending, sandboxpkg.StatusCleanedUp:
		return true, river.JobCancel(fmt.Errorf("sandbox %s ended as %s before it came up", sbx.ID, sbx.Status))
	}
	return false, nil
}

// movedUnder is what a step does when its guarded update changed nothing: the
// sandbox moved since the job read it (a delete, the TTL, the creation timeout,
// the event handler). It reads the sandbox again and reports what it became:
// nil if it is running, a cancellation if it ended, and otherwise an error so
// River retries the job from the sandbox's real status. Returning nil here
// would complete the job for a sandbox that did not come up.
func (w *CreateSandboxWorker) movedUnder(ctx context.Context, sandboxID, step string, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, err)
	}
	if done, err := settled(sbx); done {
		return err
	}
	return w.maybeCleanup(ctx, sandboxID, isFinal, job, fmt.Errorf("sandbox %s moved to %s during %s", sandboxID, sbx.Status, step))
}

// stepScheduling transitions pending → scheduling.
func (w *CreateSandboxWorker) stepScheduling(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	_, n, err := w.sandboxRepo.UpdateStatus(ctx, sandboxID, sandboxpkg.StatusScheduling, sandboxpkg.StatusPending)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, fmt.Errorf("set scheduling: %w", err))
	}
	if n == 0 {
		return w.movedUnder(ctx, sandboxID, "scheduling", isFinal, job)
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
		return w.stepStartSandbox(ctx, sbx, isFinal, job)
	}

	workers, err := w.workerRepo.List(ctx)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("list workers: %w", err))
	}

	// The load info behind candidates is a snapshot, so the chosen worker may
	// have filled up since. AssignWorker is the authoritative check: when it
	// reports the worker full, drop it and choose again among the others.
	candidates := buildCandidates(ctx, workers)
	var assigned sandboxpkg.Sandbox
	droppedFull := false
	for {
		chosen, err := w.scheduler.ChooseLeastUsedWorker(candidates)
		if errors.Is(err, ErrClusterFull) {
			// Retrying with backoff won't free capacity fast enough to be useful
			// and the HTTP caller is waiting: fail fast, treat as final.
			return w.maybeCleanup(ctx, sbx.ID, true, job, fmt.Errorf("choose worker: %w", err))
		}
		if err != nil {
			if droppedFull && errors.Is(err, ErrNoWorkerCandidates) {
				// Every candidate was dropped for being full.
				return w.maybeCleanup(ctx, sbx.ID, true, job, fmt.Errorf("choose worker: %w", ErrClusterFull))
			}
			return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("choose worker: %w", err))
		}

		assigned, err = w.sandboxRepo.AssignWorker(ctx, sbx.ID, chosen.Worker.ID)
		if errors.Is(err, sandboxpkg.ErrWorkerFull) {
			droppedFull = true
			candidates = slices.DeleteFunc(candidates, func(c WorkerCandidate) bool {
				return c.Worker.ID == chosen.Worker.ID
			})
			continue
		}
		if err != nil {
			return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("assign worker: %w", err))
		}
		break
	}
	if assigned.WorkerID == nil {
		// Guard rejected the update: the status moved since the job read it.
		return w.movedUnder(ctx, sbx.ID, "assignment", isFinal, job)
	}

	return w.stepStartSandbox(ctx, assigned, isFinal, job)
}

// stepStartSandbox calls the worker gRPC and transitions assigned → starting.
func (w *CreateSandboxWorker) stepStartSandbox(ctx context.Context, sbx sandboxpkg.Sandbox, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	if sbx.WorkerID == nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, errors.New("sandbox has no worker_id at assigned step"))
	}

	// Start the timer to measure how long it takes to start the sandbox. This is useful for monitoring and debugging.
	startTime := time.Now()

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

	callCtx, cancel := context.WithTimeout(ctx, createSandboxCallTimeout)
	defer cancel()
	_, grpcErr := client.CreateSandbox(callCtx, &workerv1.CreateSandboxRequest{
		Id:         sbx.ID,
		Runtime:    sbx.Runtime,
		Image:      sbx.Image,
		Port:       sbx.Port,
		Ttl:        sbx.TTL,
		PreviewUrl: sbx.PreviewURL,
		VcpuCount:  sbx.VcpuCount,
		MemSizeMib: sbx.MemSizeMib,
	})
	if grpcErr != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("worker CreateSandbox: %w", grpcErr))
	}

	// Measure the time it takes to start the sandbox.
	slog.Info("create_sandbox: sandbox started", "sandbox_id", sbx.ID, "duration", time.Since(startTime).String())

	_, n, err := w.sandboxRepo.UpdateStatus(ctx, sbx.ID, sandboxpkg.StatusStarting, sandboxpkg.StatusAssigned)
	if err != nil {
		return w.maybeCleanup(ctx, sbx.ID, isFinal, job, fmt.Errorf("set starting: %w", err))
	}
	if n == 0 {
		slog.Info("create_sandbox: status guard rejected starting update, may have been advanced by event handler", "sandbox_id", sbx.ID)
	}

	return w.stepAcknowledge(ctx, sbx.ID, isFinal, job)
}

// stepAcknowledge transitions starting → running (the event handler may have done this already).
func (w *CreateSandboxWorker) stepAcknowledge(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs]) error {
	sbx, err := w.sandboxRepo.GetByID(ctx, sandboxID)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, err)
	}
	// Running: the event handler already flipped it. Ended: it died under us.
	if done, err := settled(sbx); done {
		return err
	}

	_, n, err := w.sandboxRepo.UpdateStatus(ctx, sandboxID, sandboxpkg.StatusRunning,
		sandboxpkg.StatusStarting)
	if err != nil {
		return w.maybeCleanup(ctx, sandboxID, isFinal, job, fmt.Errorf("set running: %w", err))
	}
	if n == 0 {
		return w.movedUnder(ctx, sandboxID, "acknowledgement", isFinal, job)
	}
	return nil
}

// maybeCleanup: on the final attempt, enqueue a cleanup job and mark the sandbox;
// otherwise just return the error so river retries.
func (w *CreateSandboxWorker) maybeCleanup(ctx context.Context, sandboxID string, isFinal bool, job *river.Job[CreateSandboxArgs], origErr error) error {
	if !isFinal {
		return origErr
	}

	slog.Error("create_sandbox: final attempt failed, enqueueing cleanup", "sandbox_id", sandboxID, "err", origErr)

	tx, txErr := w.pool.Begin(ctx)
	if txErr != nil {
		slog.Error("create_sandbox: could not begin cleanup tx", "sandbox_id", sandboxID, "err", txErr)
		return origErr
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, _, updateErr := w.sandboxRepo.WithTx(tx).UpdateStatus(ctx, sandboxID, sandboxpkg.StatusCleanupPending,
		sandboxpkg.StatusPending, sandboxpkg.StatusScheduling, sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning)
	if updateErr != nil {
		slog.Error("create_sandbox: could not set cleanup_pending", "sandbox_id", sandboxID, "err", updateErr)
		return origErr
	}

	riverClient := river.ClientFromContext[pgx.Tx](ctx)
	_, insertErr := riverClient.InsertTx(ctx, tx, CleanupSandboxArgs{SandboxID: sandboxID}, &river.InsertOpts{Queue: "cleanup"})
	if insertErr != nil {
		slog.Error("create_sandbox: could not insert cleanup job", "sandbox_id", sandboxID, "err", insertErr)
		return origErr
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		slog.Error("create_sandbox: cleanup tx commit failed", "sandbox_id", sandboxID, "err", commitErr)
		return origErr
	}

	return river.JobCancel(fmt.Errorf("max attempts exhausted for sandbox=%s: %w", sandboxID, origErr))
}

// buildCandidates contacts each worker via gRPC to fetch current load info.
func buildCandidates(ctx context.Context, workers []workerpkg.Worker) []WorkerCandidate {
	gctx, cancel := context.WithTimeout(ctx, candidatesTimeout)
	defer cancel()

	candidates := make([]WorkerCandidate, 0, len(workers))
	for _, w := range workers {
		// Defensive liveness check: skip workers whose heartbeat has gone stale
		// before the reaper has flipped their status. This also avoids wasting
		// the 5s GetWorkerInfo timeout below on a worker that is already dead.
		// A zero timeout uses the package default (15s).
		if workerpkg.HeartbeatExpired(w.Last_heartbeat, 0) {
			continue
		}
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
