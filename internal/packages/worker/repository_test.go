//go:build integration

package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github/nallanos/fire2/internal/testutil"
)

func TestWorkerRepository_FencingToken(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	w := Worker{
		ID:       "worker-fenced",
		Status:   WorkerStatusActive,
		Address:  "10.0.0.1",
		Capacity: 4,
		Port:     5000,
	}
	first, err := repo.Register(ctx, w)
	if err != nil {
		t.Fatalf("register new worker: %v", err)
	}
	if first.FencingToken != 1 {
		t.Fatalf("first fencing token: want 1, got %d", first.FencingToken)
	}

	w.Address = "10.0.0.2"
	second, err := repo.Register(ctx, w)
	if err != nil {
		t.Fatalf("register replacement worker: %v", err)
	}
	if second.FencingToken != 2 || second.Address != w.Address {
		t.Fatalf("replacement registration: want token 2 and address %q, got token %d and address %q", w.Address, second.FencingToken, second.Address)
	}

	first.Address = "10.0.0.3"
	if _, err := repo.Update(ctx, first); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale heartbeat: want ErrFenced, got %v", err)
	}

	got, err := repo.Get(ctx, w.ID)
	if err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got.FencingToken != 2 || got.Address != w.Address {
		t.Fatalf("stale heartbeat changed row: token=%d address=%q", got.FencingToken, got.Address)
	}

	second.Capacity = 8
	updated, err := repo.Update(ctx, second)
	if err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if updated.FencingToken != 2 || updated.Capacity != 8 {
		t.Fatalf("current heartbeat: want token 2 and capacity 8, got token %d and capacity %d", updated.FencingToken, updated.Capacity)
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list workers: %v", err)
	}
	active, err := repo.ListActive(ctx)
	if err != nil {
		t.Fatalf("list active workers: %v", err)
	}
	if len(all) != 1 || all[0].FencingToken != 2 || len(active) != 1 || active[0].FencingToken != 2 {
		t.Fatalf("list scans lost fencing token: all=%+v active=%+v", all, active)
	}
}

func TestWorkerRepository_RegisterConcurrent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	const registrations = 8
	var wg sync.WaitGroup
	results := make(chan int64, registrations)
	errs := make(chan error, registrations)
	for i := 0; i < registrations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registered, err := repo.Register(ctx, Worker{
				ID: "worker-concurrent", Status: WorkerStatusActive, Address: "10.0.0.1",
			})
			if err != nil {
				errs <- err
				return
			}
			results <- registered.FencingToken
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent registration: %v", err)
	}
	seen := make(map[int64]bool)
	for token := range results {
		if token < 1 || token > registrations || seen[token] {
			t.Errorf("unexpected or duplicate fencing token %d", token)
		}
		seen[token] = true
	}
	if len(seen) != registrations {
		t.Fatalf("want %d distinct tokens, got %d", registrations, len(seen))
	}
}

// A worker may register before its port is known: Port 0 must round-trip as a
// SQL NULL (read back as 0), and a later update with a real port must persist.
func TestWorkerRepository_NullablePortRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	created, err := repo.Create(ctx, Worker{
		ID:           "worker-p0",
		Status:       WorkerStatusActive,
		Address:      "10.0.0.1",
		Capacity:     4,
		Port:         0, // not reported yet -> stored as NULL
		FencingToken: 23,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Port != 0 {
		t.Fatalf("expected port 0 (NULL), got %d", created.Port)
	}
	if created.FencingToken != 23 {
		t.Fatalf("expected explicit fencing token 23, got %d", created.FencingToken)
	}

	got, err := repo.Get(ctx, "worker-p0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Port != 0 {
		t.Fatalf("expected port 0 read back, got %d", got.Port)
	}
	if got.FencingToken != 23 {
		t.Fatalf("expected fencing token 23 read back, got %d", got.FencingToken)
	}

	got.Port = 54321
	updated, err := repo.Update(ctx, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Port != 54321 {
		t.Fatalf("expected port 54321 after update, got %d", updated.Port)
	}
}

// MarkStaleInactive must demote only active workers whose last heartbeat is
// older than the timeout, leaving fresh workers and already-inactive rows alone.
func TestWorkerRepository_MarkStaleInactive(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	now := time.Now().UTC()
	mustCreate := func(id string, status WorkerStatus, hb time.Time) {
		if _, err := repo.Create(ctx, Worker{
			ID:             id,
			Status:         status,
			Address:        "10.0.0.1",
			Capacity:       4,
			Port:           5000,
			Last_heartbeat: hb,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	mustCreate("stale-active", WorkerStatusActive, now.Add(-30*time.Second))
	mustCreate("fresh-active", WorkerStatusActive, now.Add(-2*time.Second))
	mustCreate("stale-inactive", WorkerStatusInactive, now.Add(-30*time.Second))

	n, err := repo.MarkStaleInactive(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleInactive: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 worker reaped, got %d", n)
	}

	wantStatus := map[string]WorkerStatus{
		"stale-active":   WorkerStatusInactive, // reaped
		"fresh-active":   WorkerStatusActive,   // heartbeat still fresh
		"stale-inactive": WorkerStatusInactive, // already inactive, unchanged
	}
	for id, want := range wantStatus {
		got, err := repo.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("worker %s: expected status %q, got %q", id, want, got.Status)
		}
	}

	// A second sweep is a no-op now that the stale worker is already inactive.
	n, err = repo.MarkStaleInactive(ctx, 15*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleInactive (2nd): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 reaped on second sweep, got %d", n)
	}
}
