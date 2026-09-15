package worker

import (
	"context"
	"log"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// sandboxEventPollInterval is how often Run samples every running sandbox's
// instance info. Firecracker has no push-based event stream like Docker's —
// this is a poll loop, not a reconnect delay.
const sandboxEventPollInterval = 2 * time.Second

type EventReporter struct {
	client   orchestratorv1.OrchestratorServiceClient
	workerID string
	worker   *WorkerService
}

// NewEventReporter builds a reporter that periodically samples every sandbox
// tracked by worker (see WorkerService.RunningSandboxClients) and forwards
// its instance state to the orchestrator.
func NewEventReporter(client orchestratorv1.OrchestratorServiceClient, workerID string, worker *WorkerService) *EventReporter {
	return &EventReporter{client: client, workerID: workerID, worker: worker}
}

// Run polls every running sandbox's instance info once per
// sandboxEventPollInterval and reports it to the orchestrator, until ctx is
// canceled. A single sandbox failing to report (e.g. a VM that just exited)
// only logs and is skipped — it never stops the loop or affects the others.
func (r *EventReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(sandboxEventPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reportAll(ctx)
		}
	}
}

func (r *EventReporter) reportAll(ctx context.Context) {
	for sandboxID, client := range r.worker.RunningSandboxClients() {
		event, err := r.instanceInfoEvent(ctx, client)
		if err != nil {
			log.Printf("event reporter: sandbox %s: describe instance info: %v", sandboxID, err)
			continue
		}
		if _, err := r.client.IngestSandboxEvent(ctx, event); err != nil {
			log.Printf("event reporter: sandbox %s: ingest event: %v", sandboxID, err)
		}
	}
}

// instanceInfoEvent samples client's current instance info and wraps it into
// a SandboxEvent ready to send to the orchestrator (event id, worker id and
// timestamp are this reporter's job, not the runtime client's).
func (r *EventReporter) instanceInfoEvent(ctx context.Context, client *runtimeClient.Client) (*orchestratorv1.SandboxEvent, error) {
	event, err := client.GetMetrics(ctx)
	if err != nil {
		return nil, err
	}

	return &orchestratorv1.SandboxEvent{
		Id:         uuid.NewString(),
		SandboxId:  event.SandboxId,
		WorkerId:   r.workerID,
		State:      event.State,
		OccurredAt: timestamppb.New(time.Now().UTC()),
	}, nil
}
