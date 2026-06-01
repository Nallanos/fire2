package tests

import (
	"context"
	"math/rand"
	"net"
	"strconv"
	"testing"
	"time"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	orchestrator "github/nallanos/fire2/internal/packages/orchestrator"
	workerpkg "github/nallanos/fire2/internal/packages/worker"

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

func (f *fakeWorkerServer) CreateSandbox(_ context.Context, _ *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	f.createRuns++
	return &workerv1.CreateSandboxResponse{}, nil
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

	workers := []workerpkg.Worker{
		{ID: "busy", Address: busyHost, Port: int(busyPort), Status: workerpkg.WorkerStatusActive},
		{ID: "free", Address: freeHost, Port: int(freePort), Status: workerpkg.WorkerStatusActive},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	scheduler := orchestrator.NewSchedulerWithRand(rand.New(rand.NewSource(42)))

	// The scheduler is weighted-random: free (cpu/mem=20%) should win significantly
	// more often than busy (cpu/mem=90%). Run 20 rounds and assert free > busy.
	const rounds = 20
	for i := 0; i < rounds; i++ {
		id := "sandbox-" + strconv.Itoa(i)
		_, err := orchestrator.CreateSandboxOnLeastUsedWorkerWithScheduler(ctx, scheduler, workers, &workerv1.CreateSandboxRequest{
			Id:      id,
			Runtime: "node",
			Image:   "node:20-alpine",
			Port:    3000,
			Ttl:     60,
		})
		if err != nil {
			t.Fatalf("round %d: create sandbox on least-used worker failed: %v", i, err)
		}
	}

	if free.createRuns <= busy.createRuns {
		t.Fatalf("expected free worker to receive more calls than busy: free=%d busy=%d",
			free.createRuns, busy.createRuns)
	}
}
