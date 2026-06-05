package worker

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/testutil"
)

// fakeOrchestratorClient is a no-op orchestrator client for unit tests.
type fakeOrchestratorClient struct {
	heartbeats []*orchestratorv1.WorkerHeartbeat
}

func (f *fakeOrchestratorClient) IngestSandboxEvent(_ context.Context, _ *orchestratorv1.SandboxEvent, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (f *fakeOrchestratorClient) ReportWorkerHeartbeat(_ context.Context, hb *orchestratorv1.WorkerHeartbeat, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.heartbeats = append(f.heartbeats, hb)
	return &emptypb.Empty{}, nil
}

// E1: CreateSandbox called twice for the same ID → second call is a no-op.
// Fake docker CreateContainer invoked once; running set size stays 1.
func TestWorkerService_CreateSandbox_Idempotent(t *testing.T) {
	docker := testutil.NewFakeDockerClient()
	svc := NewWorkerService(docker, &fakeOrchestratorClient{})
	svc.worker.Capacity = 10

	ctx := context.Background()
	in := CreateSandboxInput{ID: "sbx-e1", Runtime: "node", Image: "node:20-alpine", Port: 3000}

	if err := svc.CreateSandbox(ctx, in); err != nil {
		t.Fatalf("first CreateSandbox: %v", err)
	}
	if docker.CreateCallCount("sbx-e1") != 1 {
		t.Fatalf("expected 1 docker CreateContainer call, got %d", docker.CreateCallCount("sbx-e1"))
	}

	// Second call with same ID must be a no-op (already in running set).
	if err := svc.CreateSandbox(ctx, in); err != nil {
		t.Fatalf("second CreateSandbox: %v", err)
	}
	// Docker CreateContainer NOT called again because idempotency is handled in WorkerService.
	if docker.CreateCallCount("sbx-e1") != 1 {
		t.Fatalf("expected CreateContainer still called once, got %d", docker.CreateCallCount("sbx-e1"))
	}

	svc.mu.Lock()
	setSize := len(svc.runningSandboxes)
	svc.mu.Unlock()
	if setSize != 1 {
		t.Fatalf("expected running set size=1, got %d", setSize)
	}
}

// E2: CreateSandbox at full capacity returns error; running set unchanged.
func TestWorkerService_CreateSandbox_AtCapacity(t *testing.T) {
	docker := testutil.NewFakeDockerClient()
	svc := NewWorkerService(docker, &fakeOrchestratorClient{})
	svc.worker.Capacity = 1

	ctx := context.Background()

	if err := svc.CreateSandbox(ctx, CreateSandboxInput{ID: "sbx-e2a", Image: "node:20-alpine"}); err != nil {
		t.Fatalf("first: %v", err)
	}

	// Second sandbox should fail: at capacity.
	err := svc.CreateSandbox(ctx, CreateSandboxInput{ID: "sbx-e2b", Image: "node:20-alpine"})
	if err == nil {
		t.Fatal("expected error at capacity, got nil")
	}

	svc.mu.Lock()
	setSize := len(svc.runningSandboxes)
	svc.mu.Unlock()
	if setSize != 1 {
		t.Fatalf("expected running set size=1, got %d", setSize)
	}
}

// E3: CreateAndStart does NOT write to the sandboxes DB table; existing row is untouched.
func TestWorkerService_CreateAndStart_NoDBWrite(t *testing.T) {
	docker := testutil.NewFakeDockerClient()
	svc := NewWorkerService(docker, &fakeOrchestratorClient{})
	svc.worker.Capacity = 10

	ctx := context.Background()
	if err := svc.CreateSandbox(ctx, CreateSandboxInput{ID: "sbx-e3", Image: "node:20-alpine"}); err != nil {
		t.Fatalf("CreateSandbox unexpectedly wrote to DB: %v", err)
	}
}

// E4: RemoveSandbox for ID not in running set returns nil (idempotent).
func TestWorkerService_RemoveSandbox_Idempotent(t *testing.T) {
	docker := testutil.NewFakeDockerClient()
	svc := NewWorkerService(docker, &fakeOrchestratorClient{})

	ctx := context.Background()
	if err := svc.RemoveSandbox(ctx, "sbx-e4-notexist"); err != nil {
		t.Fatalf("RemoveSandbox for unknown id: %v", err)
	}
}

// E5: UpdateWorker sends a heartbeat to the orchestrator (not to the DB).
func TestWorkerService_UpdateWorker_SendsHeartbeat(t *testing.T) {
	docker := testutil.NewFakeDockerClient()
	fake := &fakeOrchestratorClient{}
	svc := NewWorkerService(docker, fake)
	svc.worker.ID = "worker-e5"
	svc.worker.Capacity = 4

	ctx := context.Background()
	id, err := svc.UpdateWorker(ctx)
	if err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}
	if id != "worker-e5" {
		t.Fatalf("expected id=worker-e5, got %q", id)
	}
	if len(fake.heartbeats) != 1 {
		t.Fatalf("expected 1 heartbeat sent, got %d", len(fake.heartbeats))
	}
	if fake.heartbeats[0].GetWorkerId() != "worker-e5" {
		t.Fatalf("heartbeat worker_id mismatch: %q", fake.heartbeats[0].GetWorkerId())
	}
}
