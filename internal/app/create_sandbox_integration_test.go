//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// C1: Happy POST → 201; sandbox row exists with status=running and worker_id populated.
func TestCreateSandbox_HappyPath(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	workerHost, workerPort := fakeWorker.StartServer(t)

	workerRepo := workerpkg.NewPostgresRepository(pool)
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:             "wk-c1",
		Status:         workerpkg.WorkerStatusActive,
		Address:        workerHost,
		Port:           workerPort,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity:       4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool)
	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	sbx := createSandbox(t, srv.URL, testutil.AuthHeader(t, pool))
	if sbx.ID == "" {
		t.Fatal("expected sandbox id in response")
	}
	if sbx.Status != "running" {
		t.Fatalf("expected status=running, got %s", sbx.Status)
	}
	if sbx.WorkerID == nil {
		t.Fatal("expected worker_id in response")
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	dbSbx, err := sandboxRepo.GetByID(ctx, sbx.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if string(dbSbx.Status) != sbx.Status {
		t.Fatalf("DB status %s != response status %s", dbSbx.Status, sbx.Status)
	}
}

// C2: river client is nil → 500; no sandbox row in DB (handler returns before creating the row).
func TestCreateSandbox_NilRiverClient_500(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	workerRepo := workerpkg.NewPostgresRepository(pool)
	_, _ = workerRepo.Create(ctx, workerpkg.Worker{
		ID:             "wk-c2",
		Status:         workerpkg.WorkerStatusActive,
		Address:        "127.0.0.1",
		Port:           50099,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 1, Mem_budget: 1024},
		Capacity:       1,
		Last_heartbeat: time.Now().UTC(),
	})

	// nil riverClient → handler returns 500 before tx starts.
	app := New(Config{Port: "0"}, pool, nil)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	payload := map[string]any{"runtime": "node", "ttl": 3600}
	body, _ := json.Marshal(payload)
	resp := postSandbox(t, srv.URL, testutil.AuthHeader(t, pool), body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	list, _ := sandboxRepo.List(ctx)
	if len(list) != 0 {
		t.Fatalf("expected no sandbox rows, got %d", len(list))
	}
}

// C3: POST with invalid JSON → 400, no row inserted.
func TestCreateSandbox_InvalidJSON_400(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	riverClient := setupFastRiverClient(t, ctx, pool)

	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	resp := postSandbox(t, srv.URL, testutil.AuthHeader(t, pool), []byte("not-json"))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	list, _ := sandboxRepo.List(ctx)
	if len(list) != 0 {
		t.Fatalf("expected no rows, got %d", len(list))
	}
}

// C4: POST with missing runtime → 400, no row inserted.
func TestCreateSandbox_MissingRuntime_400(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	riverClient := setupFastRiverClient(t, ctx, pool)

	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	payload := map[string]any{"image": "node:20-alpine", "ttl": 3600}
	body, _ := json.Marshal(payload)
	resp := postSandbox(t, srv.URL, testutil.AuthHeader(t, pool), body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	list, _ := sandboxRepo.List(ctx)
	if len(list) != 0 {
		t.Fatalf("expected no rows, got %d", len(list))
	}
}

// A worker counts its capacity in slots of 1 vCPU / 256 MiB: a sandbox asking
// for more is refused with 400 before any row or job exists, so a 4 GiB sandbox
// can no longer pass for one slot on a worker of 1 GiB.
func TestCreateSandbox_LargerThanASlot_400(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	riverClient := setupFastRiverClient(t, ctx, pool)

	workerRepo := workerpkg.NewPostgresRepository(pool)
	if _, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID: "wk-big", Status: workerpkg.WorkerStatusActive, Address: "127.0.0.1", Port: 50099,
		Budget: workerpkg.Worker_Budget{Cpu_budget: 2, Mem_budget: 952}, Capacity: 1, Last_heartbeat: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()
	auth := testutil.AuthHeader(t, pool)

	for name, payload := range map[string]map[string]any{
		"4 GiB":           {"runtime": "node", "mem_size_mib": 4096},
		"one MiB too big": {"runtime": "node", "mem_size_mib": workerpkg.SandboxMemMiB + 1},
		"2 vCPUs":         {"runtime": "node", "vcpu_count": workerpkg.SandboxVcpu + 1},
		"both":            {"runtime": "node", "vcpu_count": 8, "mem_size_mib": 8192},
	} {
		body, _ := json.Marshal(payload)
		resp := postSandbox(t, srv.URL, auth, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, resp.StatusCode)
		}
	}

	list, _ := sandboxpkg.NewPostgresRepository(pool).List(ctx)
	if len(list) != 0 {
		t.Fatalf("expected no rows for refused sizes, got %d", len(list))
	}
}

// The size of one slot, and anything smaller or unset, is accepted; the sandbox
// row keeps what was asked, defaults filled in.
func TestCreateSandbox_SlotSized_201(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	workerHost, workerPort := fakeWorker.StartServer(t)
	workerRepo := workerpkg.NewPostgresRepository(pool)
	if _, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID: "wk-slot", Status: workerpkg.WorkerStatusActive, Address: workerHost, Port: workerPort,
		Budget: workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096}, Capacity: 4, Last_heartbeat: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool)
	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()
	auth := testutil.AuthHeader(t, pool)

	for name, tc := range map[string]struct {
		payload      map[string]any
		vcpu, memMiB int32
	}{
		"exactly one slot": {map[string]any{"runtime": "node", "vcpu_count": workerpkg.SandboxVcpu, "mem_size_mib": workerpkg.SandboxMemMiB}, workerpkg.SandboxVcpu, workerpkg.SandboxMemMiB},
		"smaller":          {map[string]any{"runtime": "node", "mem_size_mib": 128}, workerpkg.SandboxVcpu, 128},
		"unset":            {map[string]any{"runtime": "node"}, workerpkg.SandboxVcpu, workerpkg.SandboxMemMiB},
	} {
		body, _ := json.Marshal(tc.payload)
		resp := postSandbox(t, srv.URL, auth, body)
		var created sandboxResponse
		err := json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || err != nil {
			t.Fatalf("%s: expected 201, got %d (decode: %v)", name, resp.StatusCode, err)
		}
		row, err := sandboxpkg.NewPostgresRepository(pool).GetByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("%s: GetByID: %v", name, err)
		}
		if row.VcpuCount != tc.vcpu || row.MemSizeMib != tc.memMiB {
			t.Errorf("%s: stored %d vCPU / %d MiB, want %d / %d", name, row.VcpuCount, row.MemSizeMib, tc.vcpu, tc.memMiB)
		}
	}
}

// I1: Full end-to-end happy path; GET /sandboxes/{id} returns same row.
func TestEndToEnd_HappyPath(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	workerHost, workerPort := fakeWorker.StartServer(t)

	workerRepo := workerpkg.NewPostgresRepository(pool)
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:             "wk-i1",
		Status:         workerpkg.WorkerStatusActive,
		Address:        workerHost,
		Port:           workerPort,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity:       4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool)
	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	auth := testutil.AuthHeader(t, pool)
	created := createSandbox(t, srv.URL, auth)
	if created.Status != "running" {
		t.Fatalf("expected running, got %s", created.Status)
	}

	fetched := getSandbox(t, srv.URL, auth, created.ID)
	if fetched.ID != created.ID {
		t.Fatalf("GET returned id=%s, want %s", fetched.ID, created.ID)
	}
	if fetched.Status != "running" {
		t.Fatalf("GET status=%s, want running", fetched.Status)
	}
}

// I2: POST → job fails permanently → 502; cleanup runs; GET shows status=failed, worker_id=nil.
func TestEndToEnd_JobFailure_502(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	fakeWorker := testutil.NewFakeWorkerServer(10, 10)
	fakeWorker.CreateError = errors.New("container refused")
	workerHost, workerPort := fakeWorker.StartServer(t)

	workerRepo := workerpkg.NewPostgresRepository(pool)
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:             "wk-i2",
		Status:         workerpkg.WorkerStatusActive,
		Address:        workerHost,
		Port:           workerPort,
		Budget:         workerpkg.Worker_Budget{Cpu_budget: 4, Mem_budget: 4096},
		Capacity:       4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool)
	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	payload := map[string]any{"runtime": "node", "ttl": 3600}
	body, _ := json.Marshal(payload)
	postResp := postSandbox(t, srv.URL, testutil.AuthHeader(t, pool), body)
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 after exhausted retries, got %d", postResp.StatusCode)
	}

	// Cleanup job runs asynchronously; wait for final settled state.
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	deadline := time.Now().Add(15 * time.Second)
	var sbx sandboxpkg.Sandbox
	for time.Now().Before(deadline) {
		list, _ := sandboxRepo.List(ctx)
		if len(list) > 0 {
			sbx = list[0]
			if sbx.Status == sandboxpkg.StatusFailed {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	if sbx.Status != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed status after cleanup, got %s", sbx.Status)
	}
	if sbx.WorkerID != nil {
		t.Fatalf("expected worker_id cleared, got %v", *sbx.WorkerID)
	}
}

// I6: POST when worker table is empty → 503; no row, no river job.
func TestEndToEnd_NoWorkers_503(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	riverClient := setupFastRiverClient(t, ctx, pool)

	app := New(Config{Port: "0"}, pool, riverClient)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	payload := map[string]any{"runtime": "node", "ttl": 3600}
	body, _ := json.Marshal(payload)
	resp := postSandbox(t, srv.URL, testutil.AuthHeader(t, pool), body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	list, _ := sandboxRepo.List(ctx)
	if len(list) != 0 {
		t.Fatalf("expected no rows for no-workers fast-path, got %d", len(list))
	}
}
