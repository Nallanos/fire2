package worker

import (
	"context"
	"log"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/sandboxmgr"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// eventRequestTimeout bounds one IngestSandboxEvent call, so a slow
// orchestrator cannot hold back the next state changes for long.
const eventRequestTimeout = 3 * time.Second

type EventReporter struct {
	client   orchestratorv1.OrchestratorServiceClient
	workerID string
	worker   *WorkerService
}

// NewEventReporter builds a reporter that forwards every sandbox state change
// of worker's sandbox manager to the orchestrator.
func NewEventReporter(client orchestratorv1.OrchestratorServiceClient, workerID string, worker *WorkerService) *EventReporter {
	return &EventReporter{client: client, workerID: workerID, worker: worker}
}

// Run forwards state changes until ctx is canceled. Delivery is best-effort:
// a failed send is logged and dropped, never retried. The heartbeat
// inventory carries every sandbox's state, so the orchestrator converges
// anyway; events only make it converge sooner.
func (r *EventReporter) Run(ctx context.Context) {
	changes := r.worker.sandboxes.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-changes:
			r.report(ctx, change)
		}
	}
}

func (r *EventReporter) report(ctx context.Context, change sandboxmgr.StateChange) {
	info, err := r.worker.GetWorkerInfo(ctx)
	if err != nil || info.FencingToken <= 0 {
		// Not registered yet: the orchestrator would reject unfenced events.
		return
	}

	rctx, cancel := context.WithTimeout(ctx, eventRequestTimeout)
	defer cancel()
	_, err = r.client.IngestSandboxEvent(rctx, &orchestratorv1.SandboxEvent{
		Id:           uuid.NewString(),
		SandboxId:    change.SandboxID,
		WorkerId:     r.workerID,
		State:        change.State,
		OccurredAt:   timestamppb.New(change.At),
		FencingToken: info.FencingToken,
	})
	if err != nil {
		log.Printf("event reporter: sandbox %s: ingest %s: %v", change.SandboxID, change.State, err)
	}
}
