//go:build integration

package worker

import (
	"context"
	"testing"
	"time"

	"github/nallanos/fire2/internal/testutil"
)

// A worker may register before its port is known: Port 0 must round-trip as a
// SQL NULL (read back as 0), and a later update with a real port must persist.
func TestWorkerRepository_NullablePortRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewPostgresRepository(pool)

	created, err := repo.Create(ctx, Worker{
		ID:       "worker-p0",
		Status:   WorkerStatusActive,
		Address:  "10.0.0.1",
		Capacity: 4,
		Port:     0, // not reported yet -> stored as NULL
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Port != 0 {
		t.Fatalf("expected port 0 (NULL), got %d", created.Port)
	}

	got, err := repo.Get(ctx, "worker-p0")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Port != 0 {
		t.Fatalf("expected port 0 read back, got %d", got.Port)
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
