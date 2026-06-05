//go:build integration

package orchestrator

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// seedSandbox inserts a sandbox directly for event handler tests.
func seedSandbox(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string, status sandboxpkg.Status) {
	t.Helper()
	_, err := repo.Create(ctx, sandboxpkg.Sandbox{
		ID:        id,
		Runtime:   "node",
		Status:    status,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed sandbox %s: %v", id, err)
	}
}

func sandboxStatus(t *testing.T, ctx context.Context, repo sandboxpkg.Repository, id string) sandboxpkg.Status {
	t.Helper()
	sbx, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID %s: %v", id, err)
	}
	return sbx.Status
}

func ingestEvent(t *testing.T, ctx context.Context, srv *EventGRPCServer, sandboxID, action string) {
	t.Helper()
	_, err := srv.IngestSandboxEvent(ctx, &orchestratorv1.SandboxEvent{
		SandboxId:   sandboxID,
		ContainerId: "ctr-" + sandboxID,
		WorkerId:    "wk-test",
		EventType:   "container",
		Action:      action,
	})
	if err != nil {
		t.Fatalf("IngestSandboxEvent action=%s: %v", action, err)
	}
}

// H1: die event while status=pending → status unchanged, "ignored" logged.
func TestEventGuard_DieWhilePending(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h1", sandboxpkg.StatusPending)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h1", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h1") != sandboxpkg.StatusPending {
		t.Fatalf("expected pending unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log, got: %s", buf.String())
	}
}

// H2: die while scheduling → unchanged + logged.
func TestEventGuard_DieWhileScheduling(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h2", sandboxpkg.StatusScheduling)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h2", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h2") != sandboxpkg.StatusScheduling {
		t.Fatalf("expected scheduling unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log")
	}
}

// H3: die while assigned → unchanged + logged.
func TestEventGuard_DieWhileAssigned(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h3", sandboxpkg.StatusAssigned)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h3", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h3") != sandboxpkg.StatusAssigned {
		t.Fatalf("expected assigned unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log")
	}
}

// H4: start event while starting → status becomes running, no log.
func TestEventGuard_StartWhileStarting(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h4", sandboxpkg.StatusStarting)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h4", "start")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h4") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after start event")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("unexpected 'ignored' log for valid start transition")
	}
}

// H5: die while running → status becomes failed.
func TestEventGuard_DieWhileRunning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h5", sandboxpkg.StatusRunning)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h5", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h5") != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed after die event while running")
	}
}

// H6: die while already failed → unchanged + logged.
func TestEventGuard_DieWhileFailed(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h6", sandboxpkg.StatusFailed)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	ingestEvent(t, ctx, srv, "sbx-h6", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h6") != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log")
	}
}

// H7: out-of-order die before start → die is rejected (pending), start then accepted (running→failed never)
// Simulates: sandbox in starting state, die arrives first (rejected), start arrives next (accepted).
func TestEventGuard_OutOfOrder_DieBeforeStart(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	// Start in "pending" (job not yet at starting).
	seedSandbox(t, ctx, sandboxRepo, "sbx-h7", sandboxpkg.StatusPending)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	// die arrives while pending → rejected
	ingestEvent(t, ctx, srv, "sbx-h7", "die")
	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h7") != sandboxpkg.StatusPending {
		t.Fatalf("expected pending after rejected die")
	}

	// Manually advance to starting (simulating job progress).
	_, _, err := sandboxRepo.UpdateStatus(ctx, "sbx-h7", sandboxpkg.StatusStarting, sandboxpkg.StatusPending)
	if err != nil {
		t.Fatalf("advance to starting: %v", err)
	}

	// start arrives while starting → accepted
	buf.Reset()
	ingestEvent(t, ctx, srv, "sbx-h7", "start")
	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h7") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after start event in starting state")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("start from starting should not be ignored")
	}
}

// seedWorker inserts a worker row directly for heartbeat handler tests.
func seedWorker(t *testing.T, ctx context.Context, repo workerpkg.Repository, id string) workerpkg.Worker {
	t.Helper()
	w, err := repo.Create(ctx, workerpkg.Worker{
		ID:       id,
		Status:   workerpkg.WorkerStatusActive,
		Address:  "127.0.0.1",
		Port:     50051,
		Capacity: 4,
		Budget:   workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 8192},
	})
	if err != nil {
		t.Fatalf("seed worker %s: %v", id, err)
	}
	return w
}

func workerFromRepo(t *testing.T, ctx context.Context, repo workerpkg.Repository, id string) workerpkg.Worker {
	t.Helper()
	w, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("get worker %s: %v", id, err)
	}
	return w
}

// W1: heartbeat for an existing worker → row updated with new metrics.
func TestHeartbeat_UpdatesExistingWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	seedWorker(t, ctx, workerRepo, "wk-w1")
	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:  "wk-w1",
		Status:    "active",
		Address:   "10.0.0.1",
		Port:      50052,
		Capacity:  8,
		CpuBudget: 8,
		MemBudget: 16384,
		CpuUsage:  2,
		MemUsage:  4096,
	})
	if err != nil {
		t.Fatalf("ReportWorkerHeartbeat: %v", err)
	}

	w := workerFromRepo(t, ctx, workerRepo, "wk-w1")
	if w.Address != "10.0.0.1" {
		t.Errorf("address: want 10.0.0.1, got %q", w.Address)
	}
	if w.Port != 50052 {
		t.Errorf("port: want 50052, got %d", w.Port)
	}
	if w.Capacity != 8 {
		t.Errorf("capacity: want 8, got %d", w.Capacity)
	}
	if w.Last_heartbeat.IsZero() {
		t.Error("last_heartbeat should be non-zero after heartbeat")
	}
}

// W2: heartbeat for a brand-new worker (not yet in DB) → self-registration via Create.
func TestHeartbeat_SelfRegisters_NewWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:  "wk-w2-new",
		Status:    "active",
		Address:   "192.168.1.5",
		Port:      50051,
		Capacity:  4,
		CpuBudget: 4,
		MemBudget: 8192,
	})
	if err != nil {
		t.Fatalf("ReportWorkerHeartbeat for new worker: %v", err)
	}

	w := workerFromRepo(t, ctx, workerRepo, "wk-w2-new")
	if w.Address != "192.168.1.5" {
		t.Errorf("address: want 192.168.1.5, got %q", w.Address)
	}
	if w.Status != workerpkg.WorkerStatusActive {
		t.Errorf("status: want active, got %q", w.Status)
	}
}

// W3: heartbeat with empty worker_id → InvalidArgument gRPC error.
func TestHeartbeat_EmptyWorkerID_ReturnsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{WorkerId: "   "})
	if err == nil {
		t.Fatal("expected error for empty worker_id, got nil")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("want InvalidArgument, got %v", err)
	}
}

// W4: second heartbeat from the same new worker → Update succeeds (no duplicate create).
func TestHeartbeat_SecondHeartbeat_Updates(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	hb := &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-w4", Status: "active", Address: "10.0.0.2", Port: 50051, Capacity: 4,
	}
	if _, err := srv.ReportWorkerHeartbeat(ctx, hb); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}

	hb.Capacity = 6
	if _, err := srv.ReportWorkerHeartbeat(ctx, hb); err != nil {
		t.Fatalf("second heartbeat: %v", err)
	}

	w := workerFromRepo(t, ctx, workerRepo, "wk-w4")
	if w.Capacity != 6 {
		t.Errorf("capacity after second heartbeat: want 6, got %d", w.Capacity)
	}
}
