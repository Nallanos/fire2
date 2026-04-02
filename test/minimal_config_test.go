package tests

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	orchestrator "github/nallanos/fire2/internal/packages/orchestrator"

	"google.golang.org/grpc"
)

type fakeWorkerServer struct {
	workerv1.UnimplementedWorkerServiceServer

	id         string
	cpuUsage   int32
	memUsage   int32
	createRuns int
}

func (f *fakeWorkerServer) GetWorkerInfo(context.Context, *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	return &workerv1.GetWorkerInfoResponse{
		Id:        f.id,
		Status:    "active",
		CpuBudget: 100,
		MemBudget: 100,
		Capacity:  10,
		CpuUsage:  f.cpuUsage,
		MemUsage:  f.memUsage,
	}, nil
}

func (f *fakeWorkerServer) CreateSandbox(_ context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	f.createRuns++

	return &workerv1.CreateSandboxResponse{
		Sandbox: &workerv1.Sandbox{
			Id:         f.id + "-sandbox",
			Runtime:    req.GetRuntime(),
			Status:     "running",
			Ttl:        req.GetTtl(),
			Port:       req.GetPort(),
			PreviewUrl: req.GetPreviewUrl(),
			Image:      req.GetImage(),
		},
	}, nil
}

func startFakeWorkerServer(t *testing.T, server *fakeWorkerServer) (string, int32, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake worker: %v", err)
	}

	grpcServer := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(grpcServer, server)

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	host, portRaw, err := net.SplitHostPort(lis.Addr().String())
	if err != nil {
		t.Fatalf("split host/port: %v", err)
	}

	port, err := strconv.Atoi(portRaw)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	cleanup := func() {
		grpcServer.Stop()
		_ = lis.Close()
	}

	return host, int32(port), cleanup
}

func TestCreateSandboxOnLeastUsedWorker_MinimalConfig(t *testing.T) {
	busy := &fakeWorkerServer{id: "worker-busy", cpuUsage: 90, memUsage: 90}
	free := &fakeWorkerServer{id: "worker-free", cpuUsage: 20, memUsage: 20}

	busyHost, busyPort, cleanupBusy := startFakeWorkerServer(t, busy)
	defer cleanupBusy()

	freeHost, freePort, cleanupFree := startFakeWorkerServer(t, free)
	defer cleanupFree()

	workers := []db.Worker{
		{ID: "busy", Address: busyHost, Port: busyPort, Status: "active"},
		{ID: "free", Address: freeHost, Port: freePort, Status: "active"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	resp, err := orchestrator.CreateSandboxOnLeastUsedWorker(ctx, workers, &workerv1.CreateSandboxRequest{
		Id:         "sandbox-test",
		Runtime:    "node",
		Image:      "node:20-alpine",
		Port:       3000,
		Ttl:        60,
		PreviewUrl: "http://localhost:3000",
	})
	if err != nil {
		t.Fatalf("create sandbox on least-used worker failed: %v", err)
	}

	if resp.GetSandbox() == nil {
		t.Fatalf("expected sandbox in response")
	}

	if got, want := resp.GetSandbox().GetId(), "worker-free-sandbox"; got != want {
		t.Fatalf("unexpected selected worker: got sandbox id %q, want %q", got, want)
	}

	if free.createRuns != 1 {
		t.Fatalf("expected free worker to receive one create call, got %d", free.createRuns)
	}
	if busy.createRuns != 0 {
		t.Fatalf("expected busy worker to receive zero create call, got %d", busy.createRuns)
	}
}
