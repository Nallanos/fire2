package orchestrator

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

// SandboxEvent is the domain type for a recorded Docker event.
type SandboxEvent struct {
	ID          string
	SandboxID   string
	ContainerID string
	WorkerID    string
	EventType   string
	Action      string
	ActorID     string
	Attributes  json.RawMessage
	OccurredAt  time.Time
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
		INSERT INTO sandbox_events (id, sandbox_id, container_id, worker_id, event_type, action, actor_id, attributes, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, sandbox_id, container_id, worker_id, event_type, action, actor_id, attributes, occurred_at`

	row := r.db.QueryRow(ctx, q,
		e.ID, e.SandboxID, e.ContainerID, e.WorkerID,
		e.EventType, e.Action, e.ActorID, e.Attributes, e.OccurredAt,
	)

	var out SandboxEvent
	err := row.Scan(
		&out.ID, &out.SandboxID, &out.ContainerID, &out.WorkerID,
		&out.EventType, &out.Action, &out.ActorID, &out.Attributes, &out.OccurredAt,
	)
	return out, err
}
