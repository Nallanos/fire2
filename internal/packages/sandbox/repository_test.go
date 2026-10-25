//go:build integration

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

func TestSandboxRepository_AssignWorker_RespectsCapacity(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	if _, err := pool.Exec(ctx, `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, created_at)
		VALUES ('wk-cap', 'active', '127.0.0.1', 2, 50051, 4, 4096, 0, 0, NOW(), NOW())
	`); err != nil {
		t.Fatalf("insert worker: %v", err)
	}

	const n = 8
	for i := 0; i < n; i++ {
		if _, err := repo.Create(ctx, Sandbox{
			ID: fmt.Sprintf("sbx-cap-%d", i), Runtime: "node", Status: StatusScheduling,
			Image: "node:20-alpine", Port: 3000, TTL: 3600, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// Assign concurrently: only `capacity` of them may win.
	var wg sync.WaitGroup
	var assigned, full atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := repo.AssignWorker(ctx, id, "wk-cap")
			switch {
			case err == nil:
				assigned.Add(1)
			case errors.Is(err, ErrWorkerFull):
				full.Add(1)
			default:
				t.Errorf("AssignWorker(%s): %v", id, err)
			}
		}(fmt.Sprintf("sbx-cap-%d", i))
	}
	wg.Wait()

	if assigned.Load() != 2 || full.Load() != n-2 {
		t.Fatalf("want 2 assigned and %d full, got %d assigned and %d full", n-2, assigned.Load(), full.Load())
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
		Status:    StatusCleanupPending, // ClearWorker only acts from cleanup_pending
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

// ClaimStalledCreation takes the sandboxes that have been pending or scheduling
// for too long, and only those: a young one is still being created, and one
// that reached a worker is liveness's business.
func TestSandboxRepository_ClaimStalledCreation(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	seed := func(id string, status Status, age time.Duration) {
		t.Helper()
		if _, err := repo.Create(ctx, Sandbox{
			ID: id, Runtime: "node", Status: status, Image: "node:20-alpine", Port: 3000,
			TTL: 3600, CreatedAt: time.Now().UTC().Add(-age),
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	seed("old-pending", StatusPending, 10*time.Minute)
	seed("old-scheduling", StatusScheduling, 10*time.Minute)
	seed("young-scheduling", StatusScheduling, 10*time.Second)
	seed("old-assigned", StatusAssigned, 10*time.Minute)
	seed("old-running", StatusRunning, 10*time.Minute)
	seed("old-failed", StatusFailed, 10*time.Minute)

	got, err := repo.ClaimStalledCreation(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ClaimStalledCreation: %v", err)
	}
	if len(got) != 2 || !(contains(got, "old-pending") && contains(got, "old-scheduling")) {
		t.Fatalf("claimed %v, want old-pending and old-scheduling", got)
	}
	for id, want := range map[string]Status{
		"old-pending":      StatusCleanupPending,
		"old-scheduling":   StatusCleanupPending,
		"young-scheduling": StatusScheduling,
		"old-assigned":     StatusAssigned,
		"old-running":      StatusRunning,
		"old-failed":       StatusFailed,
	} {
		if sbx, _ := repo.GetByID(ctx, id); sbx.Status != want {
			t.Errorf("%s is %s, want %s", id, sbx.Status, want)
		}
	}

	if again, err := repo.ClaimStalledCreation(ctx, 5*time.Minute); err != nil || len(again) != 0 {
		t.Fatalf("a second claim got %v, %v: a sandbox must be claimed once", again, err)
	}
	if _, err := repo.ClaimStalledCreation(ctx, 0); err == nil {
		t.Fatalf("a zero timeout would claim every pending sandbox: want an error")
	}
}

// ClaimLivenessExpired fails a sandbox its worker no longer vouches for, and
// only that: a worker that heartbeats without listing it (the worker's clock),
// or a worker inactive for longer than the worker-lost timeout. A worker the
// orchestrator has merely not heard from leaves its sandboxes alone, however
// old their last_seen_at is: nothing proves they are gone.
func TestSandboxRepository_ClaimLivenessExpired(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	// A worker registered a long time ago whose heartbeat was heardAgo ago, and,
	// when inactiveFor is not nil, that has been inactive for that long.
	worker := func(id, status string, heardAgo time.Duration, inactiveFor *time.Duration) {
		t.Helper()
		restartedWorker(t, ctx, pool, id, status, time.Hour, heardAgo, inactiveFor)
	}
	// A sandbox on a worker, last listed by it seenAgo ago (never when negative).
	sandbox := func(id string, status Status, workerID string, seenAgo time.Duration) {
		t.Helper()
		var wid *string
		if workerID != "" {
			wid = &workerID
		}
		if _, err := repo.Create(ctx, Sandbox{
			ID: id, Runtime: "node", Status: status, Image: "node:20-alpine", Port: 3000,
			TTL: 3600, CreatedAt: time.Now().UTC().Add(-time.Hour), WorkerID: wid,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if seenAgo >= 0 {
			if _, err := pool.Exec(ctx, `UPDATE sandboxes SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1`, id, seenAgo.Seconds()); err != nil {
				t.Fatalf("set last_seen_at %s: %v", id, err)
			}
		}
	}

	var (
		goneFor   = 3 * time.Minute
		recentFor = 30 * time.Second
	)
	worker("w-talking", "active", time.Second, nil)
	worker("w-gone", "inactive", goneFor, &goneFor)
	worker("w-just-lost", "inactive", recentFor, &recentFor)
	worker("w-unheard", "active", 10*time.Minute, nil) // the orchestrator heard nothing for 10 minutes
	// Restarted 1s ago after a long downtime: its first heartbeat is right behind
	// the registration, so it has not had the time to list anything.
	restartedWorker(t, ctx, pool, "w-restarted", "active", time.Second, 0, nil)

	// (a) the worker talks and does not list them.
	sandbox("a-unlisted", StatusRunning, "w-talking", 30*time.Second)
	sandbox("a-never-listed", StatusAssigned, "w-talking", -1) // created an hour ago, never listed
	sandbox("a-listed", StatusRunning, "w-talking", 5*time.Second)
	// (b) the worker is gone for good; its sandboxes were last listed when it went.
	sandbox("b-gone", StatusRunning, "w-gone", goneFor)
	sandbox("b-just-lost", StatusRunning, "w-just-lost", recentFor)
	// The orchestrator heard nothing from the worker, so it saw no listing: not a verdict.
	sandbox("unheard", StatusRunning, "w-unheard", 10*time.Minute)
	// Last listed by the process before the restart: the count starts at the
	// registration, and a sandbox that process left unlisted is for the first
	// inventory to find (Lost), not for liveness.
	sandbox("before-restart", StatusRunning, "w-restarted", 5*time.Minute)
	// Never claimed whatever their worker.
	sandbox("done", StatusFailed, "w-gone", goneFor)
	sandbox("creating", StatusScheduling, "", -1)

	claim := func(liveness, workerLost time.Duration) []string {
		t.Helper()
		got, err := repo.ClaimLivenessExpired(ctx, liveness, workerLost)
		if err != nil {
			t.Fatalf("ClaimLivenessExpired(%s, %s): %v", liveness, workerLost, err)
		}
		return got
	}
	assertClaimed := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("claimed %v, want %v", got, want)
		}
		for _, id := range want {
			if !contains(got, id) {
				t.Fatalf("claimed %v, want %v", got, want)
			}
		}
	}

	// Rule (a) alone: no worker is lost for an hour.
	assertClaimed(claim(15*time.Second, time.Hour), "a-unlisted", "a-never-listed")
	// Rule (b) alone: a sandbox is not unlisted for an hour. A worker inactive for
	// less than the timeout keeps its sandboxes.
	assertClaimed(claim(time.Hour, 2*time.Minute), "b-gone")

	for id, want := range map[string]Status{
		"a-unlisted":     StatusCleanupPending,
		"a-never-listed": StatusCleanupPending,
		"b-gone":         StatusCleanupPending,
		"a-listed":       StatusRunning,
		"b-just-lost":    StatusRunning,
		"unheard":        StatusRunning,
		"before-restart": StatusRunning,
		"done":           StatusFailed,
		"creating":       StatusScheduling,
	} {
		if sbx, _ := repo.GetByID(ctx, id); sbx.Status != want {
			t.Errorf("%s is %s, want %s", id, sbx.Status, want)
		}
	}

	// Both rules at once leave what neither claims, and claim a sandbox once.
	assertClaimed(claim(15*time.Second, 2*time.Minute))
	// A longer outage turns the just-lost worker into a lost one.
	assertClaimed(claim(15*time.Second, 10*time.Second), "b-just-lost")

	if _, err := repo.ClaimLivenessExpired(ctx, 15*time.Second, 0); err == nil {
		t.Fatalf("a zero worker-lost timeout would fail every inactive worker's sandboxes: want an error")
	}
}

// restartedWorker inserts a worker row registered registeredAgo ago, whose last
// heartbeat was heardAgo ago, and, when inactiveFor is not nil, that has been
// inactive for that long.
func restartedWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, status string, registeredAgo, heardAgo time.Duration, inactiveFor *time.Duration) {
	t.Helper()
	var inactive *float64
	if inactiveFor != nil {
		v := inactiveFor.Seconds()
		inactive = &v
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, inactive_since, registered_at)
		VALUES ($1, $2, '127.0.0.1', 4, 50051, 4, 4096, 0, 0,
		        now() - make_interval(secs => $3),
		        CASE WHEN $4::float8 IS NULL THEN NULL ELSE now() - make_interval(secs => $4) END,
		        now() - make_interval(secs => $5))`,
		id, status, heardAgo.Seconds(), inactive, registeredAgo.Seconds()); err != nil {
		t.Fatalf("insert worker %s: %v", id, err)
	}
}

func contains(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
