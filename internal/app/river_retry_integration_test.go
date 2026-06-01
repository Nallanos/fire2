//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/orchestrator"
)

// fastRetryPolicy returns a fixed 50ms backoff, making retry tests complete in
// milliseconds rather than the 60-second worst case of StrongRetryPolicy.
type fastRetryPolicy struct{}

func (p *fastRetryPolicy) NextRetry(_ *rivertype.JobRow) time.Time {
	return time.Now().UTC().Add(50 * time.Millisecond)
}

// setupFastRiverClient creates a River client with the fast retry policy and
// aggressive fetch intervals so tests complete quickly.
func setupFastRiverClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries) *river.Client[pgx.Tx] {
	t.Helper()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("rivermigrate.New: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate up: %v", err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, orchestrator.NewCreateSandboxWorker(queries))

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
		Workers:           workers,
		MaxAttempts:       5,
		RetryPolicy:       &fastRetryPolicy{},
		FetchCooldown:     10 * time.Millisecond,
		FetchPollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("riverClient.Start: %v", err)
	}

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	})

	return riverClient
}

// controllableWorkerServer fails the first failN CreateSandbox calls with a
// gRPC Internal error, then succeeds on subsequent calls.
type controllableWorkerServer struct {
	workerv1.UnimplementedWorkerServiceServer
	queries   *db.Queries
	failN     int
	mu        sync.Mutex
	callCount int
}

func (s *controllableWorkerServer) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

func (s *controllableWorkerServer) CreateSandbox(ctx context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	s.mu.Lock()
	s.callCount++
	n := s.callCount
	s.mu.Unlock()

	if n <= s.failN {
		return nil, grpcstatus.Errorf(codes.Internal, "simulated transient failure #%d", n)
	}

	sandbox, err := s.queries.UpdateSandboxRunning(ctx, db.UpdateSandboxRunningParams{
		ID:     req.GetId(),
		Status: "running",
		Port:   req.GetPort(),
		Image:  req.GetImage(),
	})
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "update sandbox running: %v", err)
	}

	return &workerv1.CreateSandboxResponse{
		Sandbox: &workerv1.Sandbox{
			Id:      sandbox.ID,
			Runtime: sandbox.Runtime,
			Status:  sandbox.Status,
			Ttl:     sandbox.Ttl,
			Port:    sandbox.Port,
			Image:   sandbox.Image,
		},
	}, nil
}

func (s *controllableWorkerServer) GetWorkerInfo(_ context.Context, _ *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	return &workerv1.GetWorkerInfoResponse{
		Id:        "worker-controllable",
		Status:    "active",
		CpuBudget: 8,
		MemBudget: 8192,
		Capacity:  8,
		CpuUsage:  0,
		MemUsage:  0,
	}, nil
}

// slowWorkerServer sleeps for the given duration before returning an error on
// CreateSandbox. Used to simulate a worker that takes longer than the handler
// timeout to respond.
type slowWorkerServer struct {
	workerv1.UnimplementedWorkerServiceServer
	delay time.Duration
}

func (s *slowWorkerServer) CreateSandbox(ctx context.Context, _ *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	select {
	case <-time.After(s.delay):
		return nil, grpcstatus.Errorf(codes.Unavailable, "simulated slow worker failure")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slowWorkerServer) GetWorkerInfo(_ context.Context, _ *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	return &workerv1.GetWorkerInfoResponse{
		Id:        "worker-slow",
		Status:    "active",
		CpuBudget: 8,
		MemBudget: 8192,
		Capacity:  8,
	}, nil
}

// startControllableWorker registers a controllableWorkerServer and returns its
// address info and the server itself.
func startControllableWorker(t *testing.T, queries *db.Queries, failN int) (*controllableWorkerServer, fakeWorkerAddress, int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &controllableWorkerServer{queries: queries, failN: failN}
	grpcServer := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	addr := lis.Addr().(*net.TCPAddr)
	return srv, fakeWorkerAddress{host: addr.IP.String(), server: grpcServer, lis: lis}, addr.Port
}

func startSlowWorker(t *testing.T, delay time.Duration) (fakeWorkerAddress, int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &slowWorkerServer{delay: delay}
	grpcServer := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	addr := lis.Addr().(*net.TCPAddr)
	return fakeWorkerAddress{host: addr.IP.String(), server: grpcServer, lis: lis}, addr.Port
}

// postSandboxRaw sends a POST /api/sandboxes and returns the raw http.Response
// without asserting on the status code.
func postSandboxRaw(t *testing.T, baseURL string) *http.Response {
	t.Helper()
	payload := map[string]any{
		"runtime": "node",
		"image":   "node:20-alpine",
		"port":    10001,
		"ttl":     3600,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(baseURL+"/api/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post sandbox: %v", err)
	}
	return resp
}

// TestRiverRetry_SuccessAfterTransientFailures verifies that the handler waits
// across retryable EventKindJobFailed events and returns 201 once the job
// eventually succeeds on its 3rd attempt.
func TestRiverRetry_SuccessAfterTransientFailures(t *testing.T) {
	ctx := context.Background()
	sqlDB, pool, cleanup := setupPostgresWithPool(t, ctx)
	defer cleanup()
	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	queries := db.New(sqlDB)

	controlledSrv, workerAddr, workerPort := startControllableWorker(t, queries, 2 /* fail first 2 */)
	defer workerAddr.stop()

	_, err := queries.CreateWorker(ctx, db.CreateWorkerParams{
		ID:            "worker-retry",
		Status:        "active",
		Address:       workerAddr.host,
		Port:          int32(workerPort),
		Capacity:      4,
		CpuBudget:     4,
		MemBudget:     4096,
		CpuUsage:      0,
		MemUsage:      0,
		LastHeartbeat: time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool, queries)

	cfg := Config{Port: "0", DatabaseURL: ""}
	a := New(cfg, sqlDB, riverClient)
	srv := httptest.NewServer(a.Router())
	defer srv.Close()

	resp := postSandboxRaw(t, srv.URL)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var sbx sandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbx); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if sbx.Status != "running" {
		t.Errorf("expected status=running, got %q", sbx.Status)
	}
	if got := controlledSrv.CallCount(); got != 3 {
		t.Errorf("expected 3 worker calls (2 failures + 1 success), got %d", got)
	}
}

// TestRiverRetry_AllAttemptsExhausted verifies that after all 5 attempts fail,
// the handler returns 502 and the sandbox status is set to "failed".
func TestRiverRetry_AllAttemptsExhausted(t *testing.T) {
	ctx := context.Background()
	sqlDB, pool, cleanup := setupPostgresWithPool(t, ctx)
	defer cleanup()
	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	queries := db.New(sqlDB)

	controlledSrv, workerAddr, workerPort := startControllableWorker(t, queries, 999 /* always fail */)
	defer workerAddr.stop()

	_, err := queries.CreateWorker(ctx, db.CreateWorkerParams{
		ID:            "worker-always-fail",
		Status:        "active",
		Address:       workerAddr.host,
		Port:          int32(workerPort),
		Capacity:      4,
		CpuBudget:     4,
		MemBudget:     4096,
		CpuUsage:      0,
		MemUsage:      0,
		LastHeartbeat: time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool, queries)

	cfg := Config{Port: "0", DatabaseURL: ""}
	a := New(cfg, sqlDB, riverClient)
	srv := httptest.NewServer(a.Router())
	defer srv.Close()

	resp := postSandboxRaw(t, srv.URL)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}

	// Sandbox must be marked failed in the DB.
	// We need the sandbox ID — parse it from a GET /api/sandboxes response.
	listResp, err := http.Get(srv.URL + "/api/sandboxes")
	if err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	defer listResp.Body.Close()
	var items []sandboxResponse
	if err := json.NewDecoder(listResp.Body).Decode(&items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one sandbox in list")
	}
	if items[0].Status != "failed" {
		t.Errorf("expected sandbox status=failed, got %q", items[0].Status)
	}

	if got := controlledSrv.CallCount(); got != 5 {
		t.Errorf("expected exactly 5 worker calls (MaxAttempts=5), got %d", got)
	}
}

// TestRiverRetry_AbandonedSandboxJobDiscarded verifies that when the sandbox is
// already marked failed (simulating a handler timeout), the CreateSandboxWorker
// guard returns JobCancel so River emits EventKindJobFailed+Discarded — not
// EventKindJobCompleted — and does not overwrite the sandbox status.
func TestRiverRetry_AbandonedSandboxJobDiscarded(t *testing.T) {
	ctx := context.Background()
	sqlDB, pool, cleanup := setupPostgresWithPool(t, ctx)
	defer cleanup()
	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	queries := db.New(sqlDB)
	riverClient := setupFastRiverClient(t, ctx, pool, queries)

	// Pre-create a sandbox then immediately mark it failed (simulate handler timeout).
	sbx, err := queries.CreateSandbox(ctx, db.CreateSandboxParams{
		ID:        uuid.NewString(),
		Runtime:   "node",
		Status:    "queued",
		Image:     "node:20-alpine",
		Port:      10001,
		Ttl:       3600,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if _, err := queries.UpdateSandbox(ctx, db.UpdateSandboxParams{ID: sbx.ID, Status: "failed"}); err != nil {
		t.Fatalf("mark sandbox failed: %v", err)
	}

	// Subscribe before inserting to avoid the race condition.
	// JobCancel → JobStateCancelled → EventKindJobCancelled (not EventKindJobFailed).
	eventCh, cancelSub := riverClient.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
	defer cancelSub()

	result, err := riverClient.Insert(ctx, orchestrator.CreateSandboxArgs{
		SandboxID: sbx.ID,
		Runtime:   "node",
		Image:     "node:20-alpine",
		Port:      10001,
		TTL:       3600,
	}, nil)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	jobID := result.Job.ID

	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-eventCh:
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if event.Job.ID != jobID {
				continue
			}
			// river.JobCancel sets state=Cancelled and emits EventKindJobCancelled.
			if event.Kind != river.EventKindJobCancelled {
				t.Fatalf("expected EventKindJobCancelled for abandoned job, got %v", event.Kind)
			}
			if event.Job.State != rivertype.JobStateCancelled {
				t.Fatalf("expected JobStateCancelled, got %v", event.Job.State)
			}
			// Sandbox status must NOT have been overwritten to "running".
			final, fetchErr := queries.GetSandbox(ctx, sbx.ID)
			if fetchErr != nil {
				t.Fatalf("get sandbox: %v", fetchErr)
			}
			if final.Status != "failed" {
				t.Errorf("expected sandbox status=failed (unchanged), got %q", final.Status)
			}
			return
		case <-timeout:
			t.Fatal("timed out waiting for abandoned job cancellation event")
		}
	}
}

// TestRiverRetry_HandlerTimeoutMarksSandboxFailed verifies the ctx.Done() branch:
// when the handler's wait window expires before the job completes, the sandbox
// is marked failed and the handler returns 502.
func TestRiverRetry_HandlerTimeoutMarksSandboxFailed(t *testing.T) {
	ctx := context.Background()
	sqlDB, pool, cleanup := setupPostgresWithPool(t, ctx)
	defer cleanup()
	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	queries := db.New(sqlDB)

	// Slow worker: takes 800ms per call — longer than the 500ms handler timeout.
	workerAddr, workerPort := startSlowWorker(t, 800*time.Millisecond)
	defer workerAddr.stop()

	_, err := queries.CreateWorker(ctx, db.CreateWorkerParams{
		ID:            "worker-slow",
		Status:        "active",
		Address:       workerAddr.host,
		Port:          int32(workerPort),
		Capacity:      4,
		CpuBudget:     4,
		MemBudget:     4096,
		CpuUsage:      0,
		MemUsage:      0,
		LastHeartbeat: time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool, queries)

	// 500ms handler timeout — fires before the 800ms slow worker responds.
	cfg := Config{Port: "0", DatabaseURL: "", SandboxWaitTimeout: 500 * time.Millisecond}
	a := New(cfg, sqlDB, riverClient)
	srv := httptest.NewServer(a.Router())
	defer srv.Close()

	start := time.Now()
	resp := postSandboxRaw(t, srv.URL)
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	// Should return well before 45 seconds (default) — confirm short timeout was used.
	if elapsed > 30*time.Second {
		t.Errorf("expected fast timeout response, got elapsed=%v", elapsed)
	}

	// Sandbox status must be marked failed.
	listResp, err := http.Get(srv.URL + "/api/sandboxes")
	if err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	defer listResp.Body.Close()
	var items []sandboxResponse
	if err := json.NewDecoder(listResp.Body).Decode(&items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected at least one sandbox in list")
	}
	if items[0].Status != "failed" {
		t.Errorf("expected sandbox status=failed after timeout, got %q", items[0].Status)
	}
}
