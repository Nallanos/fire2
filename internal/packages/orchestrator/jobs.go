package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"

	"github.com/riverqueue/river"
)

// errSandboxAbandoned is returned when the HTTP handler already marked the
// sandbox failed (timeout). Returning river.JobCancel for this condition makes
// River treat the job as cancelled (JobStateCancelled / EventKindJobCancelled),
// not completed or failed.
var errSandboxAbandoned = errors.New("sandbox already marked failed by handler")

type CreateSandboxArgs struct {
	SandboxID  string `json:"sandbox_id"`
	Runtime    string `json:"runtime"`
	Image      string `json:"image"`
	Port       int32  `json:"port"`
	TTL        int64  `json:"ttl"`
	PreviewURL string `json:"preview_url"`
}

func (a CreateSandboxArgs) Kind() string { return "create_sandbox" }

type CreateSandboxWorker struct {
	river.WorkerDefaults[CreateSandboxArgs]
	db db.Querier
}

func NewCreateSandboxWorker(querier db.Querier) *CreateSandboxWorker {
	return &CreateSandboxWorker{db: querier}
}

func (w *CreateSandboxWorker) Timeout(_ *river.Job[CreateSandboxArgs]) time.Duration {
	return 40 * time.Second
}

func (w *CreateSandboxWorker) Work(ctx context.Context, job *river.Job[CreateSandboxArgs]) error {
	args := job.Args

	// Fast-fail before touching the worker fleet: if the sandbox is already in
	// a terminal or complete state, skip the gRPC call entirely.
	current, err := w.db.GetSandbox(ctx, args.SandboxID)
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}
	switch current.Status {
	case string(sandboxpkg.StatusFailed):
		return river.JobCancel(errSandboxAbandoned)
	case string(sandboxpkg.StatusRunning), string(sandboxpkg.StatusSucceeded):
		return nil
	}

	workers, err := w.db.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	if len(workers) == 0 {
		return fmt.Errorf("no workers available")
	}

	grpcResp, workerAddr, err := CreateSandboxOnLeastUsedWorker(ctx, workers, &workerv1.CreateSandboxRequest{
		Id:         args.SandboxID,
		Runtime:    args.Runtime,
		Image:      args.Image,
		Port:       args.Port,
		Ttl:        args.TTL,
		PreviewUrl: args.PreviewURL,
	})
	if err != nil {
		return fmt.Errorf("create sandbox on worker: %w", err)
	}

	// Atomic transition: only write "running" if the sandbox is still "queued".
	// 0 rows means the HTTP handler abandoned the sandbox while the gRPC call
	// was in-flight — clean up the container we just created and cancel the job.
	sbx := grpcResp.GetSandbox()
	_, updateErr := w.db.UpdateSandboxIfQueued(ctx, db.UpdateSandboxIfQueuedParams{
		ID:     args.SandboxID,
		Status: string(sandboxpkg.StatusRunning),
		Port:   sbx.GetPort(),
		Image:  sbx.GetImage(),
	})
	if updateErr != nil {
		if errors.Is(updateErr, sql.ErrNoRows) {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			DestroySandboxOnWorker(cleanupCtx, workerAddr, args.SandboxID)
			return river.JobCancel(errSandboxAbandoned)
		}
		return fmt.Errorf("update sandbox running: %w", updateErr)
	}

	return nil
}
