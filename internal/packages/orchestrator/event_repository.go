package orchestrator

import (
	"context"
	"errors"
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
	// FencingToken is the token of the worker incarnation that emitted the event.
	FencingToken int64
}

// ErrEventFenced is returned by CreateSandboxEvent when the event's worker is
// not registered or its fencing token is no longer the worker's current one.
var ErrEventFenced = errors.New("sandbox event rejected: worker unknown or fencing token stale")

// EventRepository stores sandbox events received from workers.
type EventRepository interface {
	// CreateSandboxEvent stores e only if e.FencingToken is still the current
	// token of e.WorkerID, checked in the INSERT itself; otherwise nothing is
	// written and ErrEventFenced is returned.
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
		SELECT $1, $2, w.id, $4, $5 FROM worker w
		WHERE w.id = $3 AND w.fencing_token = $6
		RETURNING id, sandbox_id, worker_id, state, occurred_at`

	row := r.db.QueryRow(ctx, q,
		e.ID, e.SandboxID, e.WorkerID, e.State, e.OccurredAt, e.FencingToken,
	)

	var out SandboxEvent
	err := row.Scan(
		&out.ID, &out.SandboxID, &out.WorkerID, &out.State, &out.OccurredAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SandboxEvent{}, ErrEventFenced
	}
	return out, err
}
