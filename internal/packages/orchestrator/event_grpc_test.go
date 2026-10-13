//go:build integration

package orchestrator

import (
	"bytes"
	"context"
	"errors"
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

func ingestEvent(t *testing.T, ctx context.Context, srv *EventGRPCServer, sandboxID, state string) {
	t.Helper()
	_, err := srv.IngestSandboxEvent(ctx, &orchestratorv1.SandboxEvent{
		SandboxId: sandboxID,
		WorkerId:  "wk-test",
		State:     state,
	})
	if err != nil {
		t.Fatalf("IngestSandboxEvent state=%s: %v", state, err)
	}
}

// H1: die event while status=pending → status unchanged, "ignored" logged.
//
// Skipped: statusTransitionForState has no failure state anymore (see its
// doc comment) — firecracker-go-sdk's InstanceInfo.State never reports
// "died"/"crashed" the way Docker's die/kill/oom did, so there's nothing
// left to guard here. Re-enable once a real failure signal exists.
func TestEventGuard_DieWhilePending(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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
//
// Skipped: see TestEventGuard_DieWhilePending.
func TestEventGuard_DieWhileScheduling(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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
//
// Skipped: see TestEventGuard_DieWhilePending.
func TestEventGuard_DieWhileAssigned(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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

// H4: Running state while starting → status becomes running, no log.
func TestEventGuard_RunningWhileStarting(t *testing.T) {
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
	ingestEvent(t, ctx, srv, "sbx-h4", "Running")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h4") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after Running-state event")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("unexpected 'ignored' log for valid running transition")
	}
}

// H5: die while running → status becomes failed.
//
// Skipped: see TestEventGuard_DieWhilePending.
func TestEventGuard_DieWhileRunning(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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
//
// Skipped: see TestEventGuard_DieWhilePending.
func TestEventGuard_DieWhileFailed(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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
//
// Skipped: the "die" half has no Firecracker equivalent (see
// TestEventGuard_DieWhilePending). The "start"/Running half alone is
// already covered by TestEventGuard_RunningWhileStarting.
func TestEventGuard_OutOfOrder_DieBeforeStart(t *testing.T) {
	t.Skip("no Firecracker equivalent to Docker's die event — see statusTransitionForState")
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
	ingestEvent(t, ctx, srv, "sbx-h7", "Running")
	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h7") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after Running-state event in starting state")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("running from starting should not be ignored")
	}
}

// seedWorker registers a worker row directly for heartbeat handler tests.
func seedWorker(t *testing.T, ctx context.Context, repo workerpkg.Repository, id string) workerpkg.Worker {
	t.Helper()
	w, err := repo.Register(ctx, workerpkg.Worker{
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

func registerWorker(t *testing.T, ctx context.Context, srv *EventGRPCServer, id string) int64 {
	t.Helper()
	registration, err := srv.RegisterWorker(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId: id, Status: "active", Address: "10.0.0.2", Port: 50051, Capacity: 4,
	})
	if err != nil {
		t.Fatalf("RegisterWorker %s: %v", id, err)
	}
	if registration.GetFencingToken() <= 0 {
		t.Fatalf("RegisterWorker %s returned invalid fencing token %d", id, registration.GetFencingToken())
	}
	return registration.GetFencingToken()
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

	registered := seedWorker(t, ctx, workerRepo, "wk-w1")
	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:     "wk-w1",
		Status:       "active",
		Address:      "10.0.0.1",
		Port:         50052,
		Capacity:     8,
		CpuBudget:    8,
		MemBudget:    16384,
		CpuUsage:     2,
		MemUsage:     4096,
		FencingToken: registered.FencingToken,
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
	if w.FencingToken != registered.FencingToken {
		t.Errorf("fencing token changed on heartbeat: want %d, got %d", registered.FencingToken, w.FencingToken)
	}
}

// W2: registration creates a new worker and returns the persisted token.
func TestRegisterWorker_CreatesNewWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)

	registration, err := srv.RegisterWorker(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:  "wk-w2-new",
		Status:    "active",
		Address:   "192.168.1.5",
		Port:      50051,
		Capacity:  4,
		CpuBudget: 4,
		MemBudget: 8192,
	})
	if err != nil {
		t.Fatalf("RegisterWorker for new worker: %v", err)
	}
	if registration.GetFencingToken() <= 0 {
		t.Fatalf("expected positive fencing token, got %d", registration.GetFencingToken())
	}

	w := workerFromRepo(t, ctx, workerRepo, "wk-w2-new")
	if w.Address != "192.168.1.5" {
		t.Errorf("address: want 192.168.1.5, got %q", w.Address)
	}
	if w.Status != workerpkg.WorkerStatusActive {
		t.Errorf("status: want active, got %q", w.Status)
	}
	if w.FencingToken != registration.GetFencingToken() {
		t.Errorf("persisted fencing token: want %d, got %d", registration.GetFencingToken(), w.FencingToken)
	}
}

// W3: both RPCs reject a missing worker identity before touching storage.
func TestWorkerRPC_EmptyWorkerID_ReturnsInvalidArgument(t *testing.T) {
	srv := &EventGRPCServer{}
	for _, req := range []*orchestratorv1.WorkerHeartbeat{nil, &orchestratorv1.WorkerHeartbeat{WorkerId: "   "}} {
		if _, err := srv.RegisterWorker(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("RegisterWorker(%v): want InvalidArgument, got %v", req, err)
		}
		if _, err := srv.ReportWorkerHeartbeat(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("ReportWorkerHeartbeat(%v): want InvalidArgument, got %v", req, err)
		}
	}
}

// W4: repeated heartbeats with a current token update the same row.
func TestHeartbeat_SecondHeartbeat_Updates(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	token := registerWorker(t, ctx, srv, "wk-w4")

	hb := &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-w4", Status: "active", Address: "10.0.0.2", Port: 50051, Capacity: 4, FencingToken: token,
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
	if w.FencingToken != token {
		t.Errorf("fencing token changed after heartbeats: want %d, got %d", token, w.FencingToken)
	}
}

func TestHeartbeat_MissingToken_ReturnsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	srv := NewEventGRPCServer(
		sandboxpkg.NewPostgresRepository(pool), NewEventRepository(pool), workerpkg.NewPostgresRepository(pool),
	)
	registerWorker(t, ctx, srv, "wk-no-token")

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-no-token", Address: "should-not-persist",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for missing token, got %v", err)
	}
}

func TestHeartbeat_UnregisteredWorker_ReturnsFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	srv := NewEventGRPCServer(
		sandboxpkg.NewPostgresRepository(pool), NewEventRepository(pool), workerRepo,
	)

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-unregistered", FencingToken: 1,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for unregistered worker, got %v", err)
	}
	if _, err := workerRepo.Get(ctx, "wk-unregistered"); !errors.Is(err, workerpkg.ErrNotFound) {
		t.Fatalf("unregistered heartbeat unexpectedly created worker: %v", err)
	}
}

func TestHeartbeat_StaleTokenCannotModifyWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	srv := NewEventGRPCServer(
		sandboxpkg.NewPostgresRepository(pool), NewEventRepository(pool), workerRepo,
	)

	oldToken := registerWorker(t, ctx, srv, "wk-rotated")
	newToken := registerWorker(t, ctx, srv, "wk-rotated")
	if newToken <= oldToken {
		t.Fatalf("registration must advance fencing token: old=%d new=%d", oldToken, newToken)
	}
	rowSnapshot := func() string {
		t.Helper()
		var snapshot string
		if err := pool.QueryRow(ctx, `SELECT to_jsonb(w)::text FROM worker AS w WHERE id = $1`, "wk-rotated").Scan(&snapshot); err != nil {
			t.Fatalf("read worker row: %v", err)
		}
		return snapshot
	}
	before := rowSnapshot()

	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-rotated", FencingToken: oldToken, Status: "inactive",
		Address: "stale-address", Port: 7000, Capacity: 99,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for stale token, got %v", err)
	}
	if after := rowSnapshot(); after != before {
		t.Fatalf("stale heartbeat modified worker row:\nbefore=%s\nafter=%s", before, after)
	}

	if _, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId: "wk-rotated", FencingToken: newToken, Status: "active",
		Address: "current-address", Port: 50051, Capacity: 6,
	}); err != nil {
		t.Fatalf("heartbeat with current token: %v", err)
	}
	if got := workerFromRepo(t, ctx, workerRepo, "wk-rotated"); got.Address != "current-address" {
		t.Fatalf("current heartbeat did not update worker address: %q", got.Address)
	}
}

func TestReportWorkerHeartbeat_LogsReceivedHeartbeat(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerRepo)
	token := registerWorker(t, ctx, srv, "worker-hb")
	_, err := srv.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:     "worker-hb",
		Status:       "healthy",
		Address:      "127.0.0.1",
		Port:         7001,
		Capacity:     3,
		CpuBudget:    80,
		MemBudget:    512,
		CpuUsage:     12,
		MemUsage:     128,
		FencingToken: token,
	})
	if err != nil {
		t.Fatalf("ReportWorkerHeartbeat: %v", err)
	}

	if !strings.Contains(buf.String(), "received worker heartbeat") {
		t.Fatalf("expected heartbeat log, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "worker_id=worker-hb") {
		t.Fatalf("expected worker id in heartbeat log, got: %s", buf.String())
	}
}
