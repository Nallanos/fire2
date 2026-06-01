package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
)

// FastRetryPolicy retries river jobs with a 50ms delay, suitable for integration tests.
type FastRetryPolicy struct{}

func (p *FastRetryPolicy) NextRetry(_ *rivertype.JobRow) time.Time {
	return time.Now().Add(50 * time.Millisecond)
}

// SetupRiverClient creates a river client with FastRetryPolicy (MaxAttempts=5), two queues
// (default + cleanup), and the workers registered by addWorkers. It runs river migrations,
// starts the client, and registers a cleanup hook on t.
func SetupRiverClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool, addWorkers func(*river.Workers)) *river.Client[pgx.Tx] {
	t.Helper()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("rivermigrate.New: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate up: %v", err)
	}

	workers := river.NewWorkers()
	if addWorkers != nil {
		addWorkers(workers)
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
			"cleanup":          {MaxWorkers: 5},
		},
		Workers:     workers,
		MaxAttempts: 5,
		RetryPolicy: &FastRetryPolicy{},
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	if err := client.Start(ctx); err != nil {
		t.Fatalf("riverClient.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Stop(stopCtx)
	})

	return client
}
