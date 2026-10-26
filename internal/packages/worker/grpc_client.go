package worker

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	"github/nallanos/fire2/internal/packages/sandboxmgr"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const heartbeatIntervalEnv = "WORKER_HEARTBEAT_INTERVAL"

type WorkerGRPCServer struct {
	workerv1.UnimplementedWorkerServiceServer
	service *WorkerService
}

func NewWorkerGRPCServer(service *WorkerService) *WorkerGRPCServer {
	return &WorkerGRPCServer{service: service}
}

// checkSandboxID refuses an ID that is not a sandbox ID (a UUID). The worker
// runs as root and its gRPC API is not authenticated (only bound to a private
// address): the ID becomes a path it creates and removes recursively, so
// "../../etc" must be turned away at the door.
func checkSandboxID(id string) error {
	if err := runtimeClient.CheckSandboxID(id); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// Implement gRPC server methods
func (s *WorkerGRPCServer) CreateSandbox(ctx context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	if err := checkSandboxID(req.GetId()); err != nil {
		return nil, err
	}
	if err := CheckSandboxSize(req.GetVcpuCount(), req.GetMemSizeMib()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.service.CreateSandbox(ctx, CreateSandboxInput{
		ID:         req.GetId(),
		Runtime:    req.GetRuntime(),
		Image:      req.GetImage(),
		Port:       req.GetPort(),
		TTL:        req.GetTtl(),
		PreviewURL: req.GetPreviewUrl(),
		VcpuCount:  req.GetVcpuCount(),
		MemSizeMib: req.GetMemSizeMib(),
	}); err != nil {
		return nil, sandboxError(err)
	}

	return &workerv1.CreateSandboxResponse{}, nil
}

func (s *WorkerGRPCServer) StopSandbox(ctx context.Context, req *workerv1.StopSandboxRequest) (*workerv1.StopSandboxResponse, error) {
	if err := checkSandboxID(req.GetContainerId()); err != nil {
		return nil, err
	}
	if err := s.service.StopSandbox(req.GetContainerId()); err != nil {
		return nil, sandboxError(err)
	}

	return &workerv1.StopSandboxResponse{}, nil
}

func (s *WorkerGRPCServer) RemoveSandbox(ctx context.Context, req *workerv1.RemoveSandboxRequest) (*workerv1.RemoveSandboxResponse, error) {
	if err := checkSandboxID(req.GetContainerId()); err != nil {
		return nil, err
	}
	if err := s.service.RemoveSandbox(ctx, req.GetContainerId()); err != nil {
		return nil, sandboxError(err)
	}

	return &workerv1.RemoveSandboxResponse{}, nil
}

// sandboxError maps sandboxmgr's sentinel errors onto gRPC codes the
// orchestrator acts on: NotFound ends a cleanup, Unavailable is retried.
func sandboxError(err error) error {
	switch {
	case errors.Is(err, sandboxmgr.ErrSandboxNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, sandboxmgr.ErrBusy):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, sandboxmgr.ErrAtCapacity):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, runtimeClient.ErrInvalidSandboxID):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return err
	}
}

func (s *WorkerGRPCServer) ListRunningSandboxes(ctx context.Context, req *workerv1.ListRunningSandboxesRequest) (*workerv1.ListRunningSandboxesResponse, error) {
	containerIDs := s.service.ListRunningSandboxes(ctx)
	return &workerv1.ListRunningSandboxesResponse{
		ContainerIds: containerIDs,
	}, nil
}

func (s *WorkerGRPCServer) GetWorkerInfo(ctx context.Context, req *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	worker_info, err := s.service.GetWorkerInfo(ctx)
	if err != nil {
		return nil, err
	}

	return &workerv1.GetWorkerInfoResponse{
		Id:            worker_info.ID,
		Address:       worker_info.Address,
		Status:        string(worker_info.Status),
		CpuBudget:     int32(worker_info.Budget.Cpu_budget),
		MemBudget:     int32(worker_info.Budget.Mem_budget),
		Capacity:      int32(worker_info.Capacity),
		CpuUsage:      int32(worker_info.cpu_usage),
		MemUsage:      int32(worker_info.mem_usage),
		LastHeartbeat: timestamppb.New(worker_info.Last_heartbeat),
	}, nil
}

// ServeGRPC starts a gRPC server on the specified address and serves the provided WorkerServiceServer implementation. It returns an error if the server fails to start, or ErrHeartbeatRefused (wrapped) if the orchestrator refused the heartbeats for good and the server was stopped.
func ServeGRPC(address string, srv workerv1.WorkerServiceServer, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	// With ephemeral binding (:0) the OS picks the port at listen time, so read
	// the actual bound port back from the listener — it is what the worker
	// reports to the orchestrator and what the orchestrator dials.
	boundPort := lis.Addr().(*net.TCPAddr).Port
	slog.Info("worker gRPC listening", "address", lis.Addr().String())

	grpcServer := grpc.NewServer(opts...)
	workerv1.RegisterWorkerServiceServer(grpcServer, srv)

	slog.Info("worker gRPC server started", "address", lis.Addr().String())
	if workerSrv, ok := srv.(*WorkerGRPCServer); ok {
		workerSrv.service.SetListenPort(boundPort)

		heartbeatCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		slog.Info("starting heartbeat loop", "interval", heartbeatIntervalFromEnv().String())
		heartbeatErr := make(chan error, 1)
		go func() {
			if err := workerSrv.service.RunHeartbeat(heartbeatCtx, heartbeatIntervalFromEnv()); err != nil {
				// Sent before Stop, so Serve's return sees it.
				heartbeatErr <- err
				grpcServer.Stop()
			}
		}()
		serveErr := grpcServer.Serve(lis)
		select {
		case err := <-heartbeatErr:
			return err
		default:
			return serveErr
		}
	}

	return grpcServer.Serve(lis)
}

func heartbeatIntervalFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(heartbeatIntervalEnv))
	if raw == "" {
		return defaultHeartbeatInterval
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return defaultHeartbeatInterval
	}

	return parsed
}
