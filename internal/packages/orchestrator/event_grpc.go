package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

type EventGRPCServer struct {
	orchestratorv1.UnimplementedOrchestratorServiceServer
	sandboxRepo sandboxpkg.Repository
	eventRepo   EventRepository
	workerRepo  workerpkg.Repository
}

func NewEventGRPCServer(sandboxRepo sandboxpkg.Repository, eventRepo EventRepository, workerRepo workerpkg.Repository) *EventGRPCServer {
	return &EventGRPCServer{sandboxRepo: sandboxRepo, eventRepo: eventRepo, workerRepo: workerRepo}
}

// IngestSandboxEvent stores a sandbox event and updates sandbox status.
// State ownership: the create-sandbox job owns transitions pending→running.
// Events own transitions starting→running and running/starting→failed only.
func (s *EventGRPCServer) IngestSandboxEvent(ctx context.Context, req *orchestratorv1.SandboxEvent) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing sandbox event")
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

	_, err = s.eventRepo.CreateSandboxEvent(ctx, SandboxEvent{
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
		return nil, status.Errorf(codes.Internal, "store sandbox event: %v", err)
	}

	s.applyStatusFromAction(ctx, sandboxID, action)

	return &emptypb.Empty{}, nil
}

// ReportWorkerHeartbeat upserts the worker row with fresh metrics and a current
// heartbeat timestamp. The first heartbeat from a new worker acts as self-registration.
func (s *EventGRPCServer) ReportWorkerHeartbeat(ctx context.Context, req *orchestratorv1.WorkerHeartbeat) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetWorkerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	w := workerpkg.NewWorkerFromHeartbeat(workerpkg.HeartbeatParams{
		ID:        req.GetWorkerId(),
		Status:    workerpkg.WorkerStatus(req.GetStatus()),
		Address:   req.GetAddress(),
		Port:      int(req.GetPort()),
		Capacity:  int(req.GetCapacity()),
		CpuBudget: int(req.GetCpuBudget()),
		MemBudget: int(req.GetMemBudget()),
		CpuUsage:  int(req.GetCpuUsage()),
		MemUsage:  int(req.GetMemUsage()),
	})

	_, err := s.workerRepo.Update(ctx, w)
	if err != nil {
		if errors.Is(err, workerpkg.ErrNotFound) {
			if _, createErr := s.workerRepo.Create(ctx, w); createErr != nil {
				return nil, status.Errorf(codes.Internal, "register worker: %v", createErr)
			}
			return &emptypb.Empty{}, nil
		}
		return nil, status.Errorf(codes.Internal, "update worker: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// applyStatusFromAction applies a guarded status update based on the Docker action.
// Updates are only applied when the sandbox is in a state the event handler owns.
// rowsAffected=0 means the guard rejected the update (sandbox in a state the job owns).
func (s *EventGRPCServer) applyStatusFromAction(ctx context.Context, sandboxID, action string) {
	target, allowedFrom, ok := statusTransitionForAction(action)
	if !ok {
		return
	}

	_, n, err := s.sandboxRepo.UpdateStatus(ctx, sandboxID, target, allowedFrom...)
	if err != nil {
		log.Printf("update sandbox status failed: sandbox=%s action=%s err=%v", sandboxID, action, err)
		return
	}
	if n == 0 {
		log.Printf("ignored sandbox event: sandbox=%s action=%s — current status not in allowed set", sandboxID, action)
	}
}

// statusTransitionForAction returns the target status and the set of allowed
// source statuses for a given Docker action. The create-sandbox job owns all
// transitions before running; events only advance starting→running or
// flip running/starting→failed.
func statusTransitionForAction(action string) (sandboxpkg.Status, []sandboxpkg.Status, bool) {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		return sandboxpkg.StatusRunning, []sandboxpkg.Status{
			sandboxpkg.StatusStarting,
			sandboxpkg.StatusRunning,
		}, true
	case "die", "stop", "kill", "oom":
		return sandboxpkg.StatusFailed, []sandboxpkg.Status{
			sandboxpkg.StatusStarting,
			sandboxpkg.StatusRunning,
		}, true
	default:
		return "", nil, false
	}
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
