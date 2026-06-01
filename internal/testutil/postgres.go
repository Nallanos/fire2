package testutil

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// SetupPostgres starts a testcontainer Postgres, runs all schema migrations, and returns a pool.
// A cleanup hook is registered on t to stop the container and close the pool.
func SetupPostgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("fire2"),
		postgres.WithUsername("fire2"),
		postgres.WithPassword("fire2"),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		err := sqlDB.PingContext(pingCtx)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if err := ApplyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return pool
}

// ApplyMigrations runs the up section of all *.sql files in internal/db/migrations, sorted by name.
func ApplyMigrations(sqlDB *sql.DB) error {
	dir := migrationsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		sql := migrationUpSection(strings.TrimSpace(string(raw)))
		if sql == "" {
			continue
		}
		if _, err := sqlDB.Exec(sql); err != nil {
			return err
		}
	}
	return nil
}

// migrationsDir returns the absolute path to internal/db/migrations,
// resolved relative to this source file's location at compile time.
func migrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	// file = .../internal/testutil/postgres.go → go up one level to internal/, then db/migrations
	return filepath.Join(filepath.Dir(file), "..", "db", "migrations")
}

func migrationUpSection(text string) string {
	const upMarker = "-- migrate:up"
	const downMarker = "-- migrate:down"

	i := strings.Index(text, upMarker)
	if i == -1 {
		return strings.TrimSpace(text)
	}
	section := text[i+len(upMarker):]
	if j := strings.Index(section, downMarker); j != -1 {
		section = section[:j]
	}
	return strings.TrimSpace(section)
}
