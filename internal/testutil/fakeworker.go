package testutil

import (
	"context"
	"net"
	"sync"
	"testing"

	workerv1 "github/nallanos/fire2/gen/worker/v1"

	"google.golang.org/grpc"
)

// FakeWorkerServer implements WorkerServiceServer for use in tests.
// It tracks CreateSandbox call counts per sandbox ID and supports
// configurable error injection.
type FakeWorkerServer struct {
	workerv1.UnimplementedWorkerServiceServer

	mu          sync.Mutex
	createCalls map[string]int // sandboxID → number of CreateSandbox calls

	// CreateError, if non-nil, is returned from every CreateSandbox call.
	CreateError error

	// InfoResponse is returned from GetWorkerInfo; defaults to a healthy worker with capacity 10.
	InfoResponse *workerv1.GetWorkerInfoResponse
}

// NewFakeWorkerServer creates a fake server with the given CPU/mem usage reported.
func NewFakeWorkerServer(cpuUsage, memUsage int32) *FakeWorkerServer {
	return &FakeWorkerServer{
		createCalls: make(map[string]int),
		InfoResponse: &workerv1.GetWorkerInfoResponse{
			Status:    "active",
			CpuBudget: 100,
			MemBudget: 1000,
			Capacity:  10,
			CpuUsage:  cpuUsage,
			MemUsage:  memUsage,
		},
	}
}

func (f *FakeWorkerServer) CreateSandbox(_ context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	f.mu.Lock()
	f.createCalls[req.GetId()]++
	f.mu.Unlock()
	if f.CreateError != nil {
		return nil, f.CreateError
	}
	return &workerv1.CreateSandboxResponse{}, nil
}

func (f *FakeWorkerServer) GetWorkerInfo(_ context.Context, _ *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InfoResponse != nil {
		return f.InfoResponse, nil
	}
	return &workerv1.GetWorkerInfoResponse{Status: "active", CpuBudget: 100, MemBudget: 1000, Capacity: 10}, nil
}

func (f *FakeWorkerServer) RemoveSandbox(_ context.Context, _ *workerv1.RemoveSandboxRequest) (*workerv1.RemoveSandboxResponse, error) {
	return &workerv1.RemoveSandboxResponse{}, nil
}

// CreateCallCount returns how many times CreateSandbox was called for sandboxID.
func (f *FakeWorkerServer) CreateCallCount(sandboxID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls[sandboxID]
}

// StartServer starts a gRPC server for this fake worker and registers cleanup on t.
// Returns host and port.
func (f *FakeWorkerServer) StartServer(t *testing.T) (string, int) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeworker listen: %v", err)
	}

	srv := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(srv, f)

	go func() { _ = srv.Serve(lis) }()

	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	addr := lis.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}
