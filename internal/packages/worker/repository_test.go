//go:build integration

package worker

import (
	"context"
	"testing"

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
