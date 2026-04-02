package worker

import (
	"context"
	"net"
	"os"
	"strings"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"

	"google.golang.org/grpc"
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

// Implement gRPC server methods
func (s *WorkerGRPCServer) CreateSandbox(ctx context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	sandbox, err := s.service.CreateSandbox(ctx, CreateSandboxInput{
		ID:         req.GetId(),
		Runtime:    req.GetRuntime(),
		Image:      req.GetImage(),
		Port:       req.GetPort(),
		TTL:        req.GetTtl(),
		PreviewURL: req.GetPreviewUrl(),
	})
	if err != nil {
		return nil, err
	}

	return &workerv1.CreateSandboxResponse{
		Sandbox: &workerv1.Sandbox{
			Id:         sandbox.ID,
			Runtime:    sandbox.Runtime,
			Status:     sandbox.Status,
			Ttl:        sandbox.Ttl,
			CreatedAt:  timestamppb.New(sandbox.CreatedAt),
			Port:       sandbox.Port,
			PreviewUrl: sandbox.PreviewUrl,
			Image:      sandbox.Image,
		},
	}, nil
}

func (s *WorkerGRPCServer) StopSandbox(ctx context.Context, req *workerv1.StopSandboxRequest) (*workerv1.StopSandboxResponse, error) {
	if err := s.service.StopSandbox(ctx, req.GetContainerId()); err != nil {
		return nil, err
	}

	return &workerv1.StopSandboxResponse{}, nil
}

func (s *WorkerGRPCServer) RemoveSandbox(ctx context.Context, req *workerv1.RemoveSandboxRequest) (*workerv1.RemoveSandboxResponse, error) {
	if err := s.service.RemoveSandbox(ctx, req.GetContainerId()); err != nil {
		return nil, err
	}

	return &workerv1.RemoveSandboxResponse{}, nil
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

// ServeGRPC starts a gRPC server on the specified address and serves the provided WorkerServiceServer implementation. It returns an error if the server fails to start.
func ServeGRPC(address string, srv workerv1.WorkerServiceServer, opts ...grpc.ServerOption) error {
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(opts...)
	workerv1.RegisterWorkerServiceServer(grpcServer, srv)

	if workerSrv, ok := srv.(*WorkerGRPCServer); ok {
		heartbeatCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go workerSrv.service.RunHeartbeat(heartbeatCtx, heartbeatIntervalFromEnv())
		return grpcServer.Serve(lis)
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
