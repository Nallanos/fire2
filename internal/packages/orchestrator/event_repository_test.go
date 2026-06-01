//go:build integration

package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github/nallanos/fire2/internal/testutil"
)

// B6: EventRepository writes and reads back a sandbox event with JSON attributes.
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

	attrs, _ := json.Marshal(map[string]string{"exitCode": "0"})

	ev, err := repo.CreateSandboxEvent(ctx, SandboxEvent{
		ID:          "ev-1",
		SandboxID:   "sbx-ev-1",
		ContainerID: "ctr-1",
		WorkerID:    "wk-1",
		EventType:   "container",
		Action:      "die",
		ActorID:     "ctr-1",
		Attributes:  attrs,
		OccurredAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateSandboxEvent: %v", err)
	}
	if ev.ID != "ev-1" {
		t.Fatalf("unexpected id: %s", ev.ID)
	}
	if ev.Action != "die" {
		t.Fatalf("unexpected action: %s", ev.Action)
	}

	// Verify attributes round-trip.
	var decoded map[string]string
	if err := json.Unmarshal(ev.Attributes, &decoded); err != nil {
		t.Fatalf("decode attributes: %v", err)
	}
	if decoded["exitCode"] != "0" {
		t.Fatalf("unexpected exitCode: %s", decoded["exitCode"])
	}
}
