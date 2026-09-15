package orchestrator

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

// SandboxEvent is the domain type for a recorded Firecracker instance-state
// sample (see runtime.Client.GetMetrics / firecracker-go-sdk's InstanceInfo).
type SandboxEvent struct {
	ID         string
	SandboxID  string
	WorkerID   string
	State      string
	OccurredAt time.Time
}

// EventRepository stores sandbox events received from workers.
type EventRepository interface {
	CreateSandboxEvent(ctx context.Context, e SandboxEvent) (SandboxEvent, error)
	WithTx(tx pgx.Tx) EventRepository
}

type postgresEventRepository struct {
	db pgxdb.DBTX
}

func NewEventRepository(db pgxdb.DBTX) EventRepository {
	return &postgresEventRepository{db: db}
}

func (r *postgresEventRepository) WithTx(tx pgx.Tx) EventRepository {
	return &postgresEventRepository{db: tx}
}

func (r *postgresEventRepository) CreateSandboxEvent(ctx context.Context, e SandboxEvent) (SandboxEvent, error) {
	const q = `
		INSERT INTO sandbox_events (id, sandbox_id, worker_id, state, occurred_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, sandbox_id, worker_id, state, occurred_at`

	row := r.db.QueryRow(ctx, q,
		e.ID, e.SandboxID, e.WorkerID, e.State, e.OccurredAt,
	)

	var out SandboxEvent
	err := row.Scan(
		&out.ID, &out.SandboxID, &out.WorkerID, &out.State, &out.OccurredAt,
	)
	return out, err
}
