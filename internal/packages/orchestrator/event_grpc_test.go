//go:build integration

package orchestrator

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

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

func ingestEvent(t *testing.T, ctx context.Context, srv *EventGRPCServer, sandboxID, action string) {
	t.Helper()
	_, err := srv.IngestSandboxEvent(ctx, &orchestratorv1.SandboxEvent{
		SandboxId:   sandboxID,
		ContainerId: "ctr-" + sandboxID,
		WorkerId:    "wk-test",
		EventType:   "container",
		Action:      action,
	})
	if err != nil {
		t.Fatalf("IngestSandboxEvent action=%s: %v", action, err)
	}
}

// H1: die event while status=pending → status unchanged, "ignored" logged.
func TestEventGuard_DieWhilePending(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h1", sandboxpkg.StatusPending)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
	ingestEvent(t, ctx, srv, "sbx-h1", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h1") != sandboxpkg.StatusPending {
		t.Fatalf("expected pending unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log, got: %s", buf.String())
	}
}

// H2: die while scheduling → unchanged + logged.
func TestEventGuard_DieWhileScheduling(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h2", sandboxpkg.StatusScheduling)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
	ingestEvent(t, ctx, srv, "sbx-h2", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h2") != sandboxpkg.StatusScheduling {
		t.Fatalf("expected scheduling unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log")
	}
}

// H3: die while assigned → unchanged + logged.
func TestEventGuard_DieWhileAssigned(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h3", sandboxpkg.StatusAssigned)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
	ingestEvent(t, ctx, srv, "sbx-h3", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h3") != sandboxpkg.StatusAssigned {
		t.Fatalf("expected assigned unchanged")
	}
	if !strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("expected 'ignored sandbox event' in log")
	}
}

// H4: start event while starting → status becomes running, no log.
func TestEventGuard_StartWhileStarting(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h4", sandboxpkg.StatusStarting)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
	ingestEvent(t, ctx, srv, "sbx-h4", "start")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h4") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after start event")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("unexpected 'ignored' log for valid start transition")
	}
}

// H5: die while running → status becomes failed.
func TestEventGuard_DieWhileRunning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h5", sandboxpkg.StatusRunning)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
	ingestEvent(t, ctx, srv, "sbx-h5", "die")

	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h5") != sandboxpkg.StatusFailed {
		t.Fatalf("expected failed after die event while running")
	}
}

// H6: die while already failed → unchanged + logged.
func TestEventGuard_DieWhileFailed(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	seedSandbox(t, ctx, sandboxRepo, "sbx-h6", sandboxpkg.StatusFailed)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))
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
func TestEventGuard_OutOfOrder_DieBeforeStart(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)
	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	eventRepo := NewEventRepository(pool)

	// Start in "pending" (job not yet at starting).
	seedSandbox(t, ctx, sandboxRepo, "sbx-h7", sandboxpkg.StatusPending)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	srv := NewEventGRPCServer(sandboxRepo, eventRepo, workerpkg.NewPostgresRepository(pool))

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
	ingestEvent(t, ctx, srv, "sbx-h7", "start")
	if sandboxStatus(t, ctx, sandboxRepo, "sbx-h7") != sandboxpkg.StatusRunning {
		t.Fatalf("expected running after start event in starting state")
	}
	if strings.Contains(buf.String(), "ignored sandbox event") {
		t.Fatalf("start from starting should not be ignored")
	}
}
