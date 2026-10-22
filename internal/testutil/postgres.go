package testutil

import (
	"context"
	"database/sql"
	"fmt"
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

// pgReadyTimeout is how long one container gets to answer. Many may start at
// once (parallel tests) on a loaded machine.
const pgReadyTimeout = 20 * time.Second

// pgAttempts is how many containers SetupPostgres tries. A container whose
// mapped port never answers (seen under load with Docker Desktop) is thrown
// away rather than waited for.
const pgAttempts = 3

// SetupPostgres starts a testcontainer Postgres, runs all schema migrations, and returns a pool.
// A cleanup hook is registered on t to stop the container and close the pool.
func SetupPostgres(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	var pool *pgxpool.Pool
	var sqlDB *sql.DB
	var err error
	for attempt := 1; attempt <= pgAttempts; attempt++ {
		var stop func()
		pool, sqlDB, stop, err = startPostgres(ctx)
		if err == nil {
			t.Cleanup(stop)
			break
		}
		t.Logf("postgres container attempt %d/%d: %v", attempt, pgAttempts, err)
	}
	if err != nil {
		t.Fatalf("postgres not ready after %d containers: %v", pgAttempts, err)
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

// startPostgres starts one container and waits for it to answer. stop releases
// everything it started; on error nothing is left running.
func startPostgres(ctx context.Context) (pool *pgxpool.Pool, sqlDB *sql.DB, stop func(), err error) {
	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("fire2"),
		postgres.WithUsername("fire2"),
		postgres.WithPassword("fire2"),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("start container: %w", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, nil, nil, fmt.Errorf("connection string: %w", err)
	}
	pool, err = pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		return nil, nil, nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	sqlDB = stdlib.OpenDBFromPool(pool)
	stop = func() {
		_ = sqlDB.Close()
		pool.Close()
		_ = container.Terminate(ctx)
	}

	var pingErr error
	deadline := time.Now().Add(pgReadyTimeout)
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		pingErr = sqlDB.PingContext(pingCtx)
		cancel()
		if pingErr == nil {
			return pool, sqlDB, stop, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	stop()
	return nil, nil, nil, fmt.Errorf("no answer after %s: %w", pgReadyTimeout, pingErr)
}
