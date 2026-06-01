//go:build integration

package sandbox

import (
	"context"
	"testing"
	"time"

	"github/nallanos/fire2/internal/testutil"
)

func TestSandboxRepository_CreateRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	sbx, err := repo.Create(ctx, Sandbox{
		ID:        "sbx-b1",
		Runtime:   "node",
		Status:    StatusPending,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sbx.ID != "sbx-b1" {
		t.Fatalf("unexpected id %s", sbx.ID)
	}
	if sbx.WorkerID != nil {
		t.Fatalf("expected nil worker_id, got %v", *sbx.WorkerID)
	}
	if sbx.Status != StatusPending {
		t.Fatalf("expected pending status, got %s", sbx.Status)
	}
}

func TestSandboxRepository_UpdateStatus_Guard(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	_, err := repo.Create(ctx, Sandbox{
		ID:        "sbx-b2",
		Runtime:   "node",
		Status:    StatusPending,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Allowed transition: pending → scheduling
	updated, n, err := repo.UpdateStatus(ctx, "sbx-b2", StatusScheduling, StatusPending)
	if err != nil {
		t.Fatalf("UpdateStatus allowed: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected rowsAffected=1, got %d", n)
	}
	if updated.Status != StatusScheduling {
		t.Fatalf("expected scheduling, got %s", updated.Status)
	}

	// Rejected: current status is scheduling, not pending → rowsAffected=0, row unchanged
	_, n2, err := repo.UpdateStatus(ctx, "sbx-b2", StatusRunning, StatusPending)
	if err != nil {
		t.Fatalf("UpdateStatus rejected: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected rowsAffected=0 for rejected guard, got %d", n2)
	}

	// Verify row was not mutated
	fetched, err := repo.GetByID(ctx, "sbx-b2")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.Status != StatusScheduling {
		t.Fatalf("expected status unchanged at scheduling, got %s", fetched.Status)
	}
}

func TestSandboxRepository_AssignWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	_, err := repo.Create(ctx, Sandbox{
		ID:        "sbx-b3",
		Runtime:   "node",
		Status:    StatusScheduling,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Need a worker row for FK constraint.
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, created_at)
		VALUES ('wk-1', 'active', '127.0.0.1', 4, 50051, 4, 4096, 0, 0, NOW(), NOW())
	`); err != nil {
		t.Fatalf("insert worker: %v", err)
	}

	sbx, err := repo.AssignWorker(ctx, "sbx-b3", "wk-1")
	if err != nil {
		t.Fatalf("AssignWorker: %v", err)
	}
	if sbx.WorkerID == nil {
		t.Fatal("expected worker_id to be set after AssignWorker")
	}
	if *sbx.WorkerID != "wk-1" {
		t.Fatalf("expected worker_id=wk-1, got %s", *sbx.WorkerID)
	}
	if sbx.Status != StatusAssigned {
		t.Fatalf("expected status=assigned, got %s", sbx.Status)
	}
}

func TestSandboxRepository_ClearWorker(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	// Insert worker for FK.
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, created_at)
		VALUES ('wk-2', 'active', '127.0.0.1', 4, 50051, 4, 4096, 0, 0, NOW(), NOW())
	`); err != nil {
		t.Fatalf("insert worker: %v", err)
	}

	wid := "wk-2"
	_, err := repo.Create(ctx, Sandbox{
		ID:        "sbx-b4",
		Runtime:   "node",
		Status:    StatusAssigned,
		Image:     "node:20-alpine",
		Port:      3000,
		TTL:       3600,
		WorkerID:  &wid,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create with worker_id: %v", err)
	}

	if err := repo.ClearWorker(ctx, "sbx-b4"); err != nil {
		t.Fatalf("ClearWorker: %v", err)
	}

	fetched, err := repo.GetByID(ctx, "sbx-b4")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if fetched.WorkerID != nil {
		t.Fatalf("expected worker_id=null after ClearWorker, got %v", *fetched.WorkerID)
	}
}

// B7: Repository accepts both *pgxpool.Pool and pgx.Tx via DBTX interface.
func TestSandboxRepository_WithTx_RoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	txRepo := repo.WithTx(tx)
	_, err = txRepo.Create(ctx, Sandbox{
		ID:        "sbx-b7",
		Runtime:   "go",
		Status:    StatusPending,
		Image:     "golang:1.23-alpine",
		Port:      8080,
		TTL:       1800,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create in tx: %v", err)
	}

	// Visible inside tx, not yet committed.
	sbx, err := txRepo.GetByID(ctx, "sbx-b7")
	if err != nil {
		t.Fatalf("get in tx: %v", err)
	}
	if sbx.ID != "sbx-b7" {
		t.Fatalf("unexpected id %s", sbx.ID)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Now visible outside the tx too.
	sbx2, err := repo.GetByID(ctx, "sbx-b7")
	if err != nil {
		t.Fatalf("get after commit: %v", err)
	}
	if sbx2.ID != "sbx-b7" {
		t.Fatalf("unexpected id after commit: %s", sbx2.ID)
	}
}
