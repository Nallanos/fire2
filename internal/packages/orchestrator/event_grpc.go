package orchestrator

import (
	"context"
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
// Events own transitions starting→running only — see statusTransitionForState
// for why running/starting→failed isn't handled here anymore.
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
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}

	workerID := strings.TrimSpace(req.GetWorkerId())
	state := strings.TrimSpace(req.GetState())
	if workerID == "" || state == "" {
		return nil, status.Error(codes.InvalidArgument, "missing required fields")
	}

	_, err := s.eventRepo.CreateSandboxEvent(ctx, SandboxEvent{
		ID:         eventID,
		SandboxID:  sandboxID,
		WorkerID:   workerID,
		State:      state,
		OccurredAt: occurredAt,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store sandbox event: %v", err)
	}

	s.applyStatusFromState(ctx, sandboxID, state)

	return &emptypb.Empty{}, nil
}

// ReportWorkerHeartbeat upserts the worker row with fresh metrics and a current
// heartbeat timestamp. The first heartbeat from a new worker acts as self-registration.
func (s *EventGRPCServer) ReportWorkerHeartbeat(ctx context.Context, req *orchestratorv1.WorkerHeartbeat) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetWorkerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	workerStatus := workerpkg.WorkerStatus(req.GetStatus())
	switch workerStatus {
	case workerpkg.WorkerStatusActive, workerpkg.WorkerStatusInactive:
	default:
		workerStatus = workerpkg.WorkerStatusActive
	}

	log.Printf("received worker heartbeat: worker_id=%s status=%s address=%s port=%d capacity=%d cpu_usage=%d mem_usage=%d",
		req.GetWorkerId(), req.GetStatus(), req.GetAddress(), req.GetPort(), req.GetCapacity(), req.GetCpuUsage(), req.GetMemUsage())

	w := workerpkg.NewWorkerFromHeartbeat(workerpkg.HeartbeatParams{
		ID:        req.GetWorkerId(),
		Status:    workerStatus,
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

// applyStatusFromState applies a guarded status update based on the reported
// Firecracker instance state. Updates are only applied when the sandbox is in
// a state the event handler owns. rowsAffected=0 means the guard rejected the
// update (sandbox in a state the job owns).
func (s *EventGRPCServer) applyStatusFromState(ctx context.Context, sandboxID, state string) {
	target, allowedFrom, ok := statusTransitionForState(state)
	if !ok {
		return
	}

	_, n, err := s.sandboxRepo.UpdateStatus(ctx, sandboxID, target, allowedFrom...)
	if err != nil {
		log.Printf("update sandbox status failed: sandbox=%s state=%s err=%v", sandboxID, state, err)
		return
	}
	if n == 0 {
		log.Printf("ignored sandbox event: sandbox=%s state=%s — current status not in allowed set", sandboxID, state)
	}
}

// statusTransitionForState returns the target status and the set of allowed
// source statuses for a reported firecracker-go-sdk InstanceInfo.State value
// ("Not started" | "Running" | "Paused"). The create-sandbox job owns all
// transitions before running; events only advance starting→running.
//
// Unlike Docker's die/stop/kill/oom, InstanceInfo has no "crashed"/"exited"
// state to drive a starting|running→failed transition — that signal doesn't
// exist here anymore. Detecting a dead sandbox needs a different mechanism
// (e.g. missed heartbeats), not this event stream.
func statusTransitionForState(state string) (sandboxpkg.Status, []sandboxpkg.Status, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return sandboxpkg.StatusRunning, []sandboxpkg.Status{
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
