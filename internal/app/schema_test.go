//go:build integration

package app

import (
	"context"
	"testing"

	"github/nallanos/fire2/internal/testutil"
)

// A1: Migration 005 applies cleanly; worker_id column exists and is nullable.
func TestMigration_SandboxWorkerID_Exists(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	var colExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_name='sandboxes' AND column_name='worker_id'
		)
	`).Scan(&colExists); err != nil {
		t.Fatalf("query worker_id: %v", err)
	}
	if !colExists {
		t.Fatal("worker_id column not found on sandboxes")
	}

	var nullable string
	if err := pool.QueryRow(ctx, `
		SELECT is_nullable FROM information_schema.columns
		WHERE table_name='sandboxes' AND column_name='worker_id'
	`).Scan(&nullable); err != nil {
		t.Fatalf("query nullable: %v", err)
	}
	if nullable != "YES" {
		t.Fatalf("expected worker_id to be nullable, got is_nullable=%s", nullable)
	}
}

// A2: Down migration removes the column cleanly.
func TestMigration_SandboxWorkerID_DownRemovesColumn(t *testing.T) {
	ctx := context.Background()
	pool := testutil.SetupPostgres(t, ctx)

	// Apply the down migration manually.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS sandboxes_worker_id_idx`,
		`ALTER TABLE sandboxes DROP COLUMN IF EXISTS worker_id`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("down migration stmt %q: %v", stmt, err)
		}
	}

	var colExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_name='sandboxes' AND column_name='worker_id'
		)
	`).Scan(&colExists); err != nil {
		t.Fatalf("query after down: %v", err)
	}
	if colExists {
		t.Fatal("worker_id column should be removed after down migration")
	}
}
