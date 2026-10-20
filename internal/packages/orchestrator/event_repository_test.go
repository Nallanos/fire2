//go:build integration

package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// B6: EventRepository stores an event only for the worker's current fencing
// token; a stale or unknown one writes nothing.
func TestEventRepository_CreateSandboxEvent(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	repo := NewEventRepository(pool)

	// Insert a sandbox first (FK constraint).
	if _, err := pool.Exec(ctx, `
		INSERT INTO sandboxes (id, runtime, status, image, port, ttl, preview_url, created_at)
		VALUES ('sbx-ev-1', 'node', 'running', 'node:20-alpine', 3000, 3600, '', NOW())
	`); err != nil {
		t.Fatalf("insert sandbox: %v", err)
	}

	workerRepo := workerpkg.NewPostgresRepository(pool)
	oldW := seedWorker(t, ctx, workerRepo, "wk-1")
	newW := seedWorker(t, ctx, workerRepo, "wk-1") // new incarnation: token advances
	if newW.FencingToken <= oldW.FencingToken {
		t.Fatalf("token must advance: old=%d new=%d", oldW.FencingToken, newW.FencingToken)
	}

	event := func(id string, token int64, worker string) SandboxEvent {
		return SandboxEvent{
			ID: id, SandboxID: "sbx-ev-1", WorkerID: worker, State: "Running",
			OccurredAt: time.Now().UTC(), FencingToken: token,
		}
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sandbox_events`).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}

	ev, err := repo.CreateSandboxEvent(ctx, event("ev-1", newW.FencingToken, "wk-1"))
	if err != nil {
		t.Fatalf("CreateSandboxEvent: %v", err)
	}
	if ev.ID != "ev-1" || ev.State != "Running" || ev.WorkerID != "wk-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}

	for name, e := range map[string]SandboxEvent{
		"stale token":    event("ev-stale", oldW.FencingToken, "wk-1"),
		"unknown worker": event("ev-unknown", 1, "wk-nobody"),
	} {
		if _, err := repo.CreateSandboxEvent(ctx, e); !errors.Is(err, ErrEventFenced) {
			t.Fatalf("%s: want ErrEventFenced, got %v", name, err)
		}
	}
	if n := count(); n != 1 {
		t.Fatalf("fenced events must not be stored: %d rows, want 1", n)
	}
}
