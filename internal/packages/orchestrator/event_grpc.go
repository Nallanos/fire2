package orchestrator

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"strings"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/sandbox"
)

type EventGRPCServer struct {
	orchestratorv1.UnimplementedOrchestratorServiceServer
	db db.Querier
}

func NewEventGRPCServer(db db.Querier) *EventGRPCServer {
	return &EventGRPCServer{db: db}
}

// IngestSandboxEvent stores a sandbox event from a worker and updates sandbox status based on the event action.
func (s *EventGRPCServer) IngestSandboxEvent(ctx context.Context, req *orchestratorv1.SandboxEvent) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing docker event")
	}

	eventID := strings.TrimSpace(req.GetId())
	if eventID == "" {
		eventID = uuid.NewString()
	}

	occurredAt := time.Now().UTC()
	if ts := req.GetOccurredAt(); ts != nil {
		occurredAt = ts.AsTime().UTC()
	}

	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if sandboxID == "" {
		sandboxID = req.GetAttributes()["sandbox_id"]
		if sandboxID == "" {
			sandboxID = req.GetAttributes()["id"]
		}
	}
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}

	containerID := strings.TrimSpace(req.GetContainerId())
	if containerID == "" {
		containerID = strings.TrimSpace(req.GetActorId())
	}

	workerID := strings.TrimSpace(req.GetWorkerId())
	eventType := strings.TrimSpace(req.GetEventType())
	action := strings.TrimSpace(req.GetAction())
	if containerID == "" || workerID == "" || eventType == "" || action == "" {
		return nil, status.Error(codes.InvalidArgument, "missing required fields")
	}

	attrsJSON, err := json.Marshal(req.GetAttributes())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid attributes: %v", err)
	}

	_, err = s.db.CreateSandboxEvent(ctx, db.CreateSandboxEventParams{
		ID:          eventID,
		SandboxID:   sandboxID,
		ContainerID: containerID,
		WorkerID:    workerID,
		EventType:   eventType,
		Action:      action,
		ActorID:     req.GetActorId(),
		Attributes:  attrsJSON,
		OccurredAt:  occurredAt,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store docker event: %v", err)
	}

	if statusValue, ok := sandboxStatusForAction(action); ok {
		if _, updateErr := s.db.UpdateSandbox(ctx, db.UpdateSandboxParams{
			ID:     sandboxID,
			Status: statusValue,
		}); updateErr != nil {
			log.Printf("update sandbox status failed: sandbox=%s action=%s err=%v", sandboxID, action, updateErr)
		}
	}

	return &emptypb.Empty{}, nil
}

func ServeEventGRPC(address string, srv orchestratorv1.OrchestratorServiceServer, opts ...grpc.ServerOption) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(opts...)
	orchestratorv1.RegisterOrchestratorServiceServer(grpcServer, srv)
	return grpcServer.Serve(listener)
}

type EventClient struct {
	conn   *grpc.ClientConn
	client orchestratorv1.OrchestratorServiceClient
}

func NewEventClient(ctx context.Context, address string) (*EventClient, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &EventClient{conn: conn, client: orchestratorv1.NewOrchestratorServiceClient(conn)}, nil
}

func (c *EventClient) Client() orchestratorv1.OrchestratorServiceClient {
	return c.client
}

func (c *EventClient) Close() error {
	return c.conn.Close()
}

func sandboxStatusForAction(action string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		return string(sandbox.StatusRunning), true
	case "die", "stop", "kill", "oom":
		return string(sandbox.StatusFailed), true
	default:
		return "", false
	}
}
