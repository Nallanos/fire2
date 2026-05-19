package worker

import (
	"context"
	"log"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github/nallanos/fire2/internal/packages/docker"
)

const sandboxEventRetryDelay = 2 * time.Second

type EventReporter struct {
	docker   docker.ClientInterface
	client   orchestratorv1.OrchestratorServiceClient
	workerID string
}

func NewEventReporter(dockerClient docker.ClientInterface, client orchestratorv1.OrchestratorServiceClient, workerID string) *EventReporter {
	return &EventReporter{docker: dockerClient, client: client, workerID: workerID}
}

func (r *EventReporter) Run(ctx context.Context) {
	for {
		eventsCh, errCh := r.docker.Events(ctx)
		streamOK := true
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-errCh:
				if ok && err != nil {
					log.Printf("sandbox events stream error: %v", err)
				}
				streamOK = false
			case event, ok := <-eventsCh:
				if !ok {
					streamOK = false
					continue
				}
				if event.Type != "container" {
					continue
				}
				payload, err := sandboxEventPayload(r.workerID, event)
				if err != nil {
					log.Printf("sandbox event payload error: %v", err)
					continue
				}
				if _, err := r.client.IngestSandboxEvent(ctx, payload); err != nil {
					log.Printf("report sandbox event failed: %v", err)
				}
			}
			if !streamOK {
				time.Sleep(sandboxEventRetryDelay)
				break
			}
		}
	}
}

func sandboxEventPayload(workerID string, event docker.EventMessage) (*orchestratorv1.SandboxEvent, error) {
	sandboxID := event.Actor.Attributes["sandbox_id"]
	if sandboxID == "" {
		sandboxID = event.Actor.Attributes["id"]
	}

	attrs := map[string]string{}
	for k, v := range event.Actor.Attributes {
		attrs[k] = v
	}

	return &orchestratorv1.SandboxEvent{
		Id:          uuid.NewString(),
		SandboxId:   sandboxID,
		ContainerId: event.Actor.ID,
		WorkerId:    workerID,
		EventType:   event.Type,
		Action:      event.Action,
		ActorId:     event.Actor.ID,
		Attributes:  attrs,
		OccurredAt:  timestamppb.New(time.Unix(event.Time, 0).UTC()),
	}, nil
}
