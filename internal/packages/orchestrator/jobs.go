package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/riverqueue/river"
	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

// errSandboxAbandoned is returned when the HTTP handler already marked the
// sandbox failed (timeout). River treats this as a non-retriable discard so
// it emits EventKindJobFailed rather than EventKindJobCompleted.
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

	// Guard: if the HTTP handler timed out and already marked the sandbox failed,
	// don't overwrite that state with a successful result.
	current, err := w.db.GetSandbox(ctx, args.SandboxID)
	if err == nil && current.Status == string(sandboxpkg.StatusFailed) {
		return river.JobCancel(errSandboxAbandoned)
	}

	workers, err := w.db.ListWorkers(ctx)
	if err != nil {
		return fmt.Errorf("list workers: %w", err)
	}
	if len(workers) == 0 {
		return fmt.Errorf("no workers available")
	}

	grpcResp, err := CreateSandboxOnLeastUsedWorker(ctx, workers, &workerv1.CreateSandboxRequest{
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

	sbx := grpcResp.GetSandbox()
	if _, updateErr := w.db.UpdateSandboxRunning(ctx, db.UpdateSandboxRunningParams{
		ID:     args.SandboxID,
		Status: "running",
		Port:   sbx.GetPort(),
		Image:  sbx.GetImage(),
	}); updateErr != nil {
		log.Printf("update sandbox running: id=%s err=%v", args.SandboxID, updateErr)
	}

	return nil
}
