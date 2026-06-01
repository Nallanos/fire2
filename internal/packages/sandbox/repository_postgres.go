package sandbox

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

// Repository is the storage interface for sandboxes.
type Repository interface {
	Create(ctx context.Context, s Sandbox) (Sandbox, error)
	GetByID(ctx context.Context, id string) (Sandbox, error)
	List(ctx context.Context) ([]Sandbox, error)
	// UpdateStatus advances status only when current status is one of allowedFrom.
	// Returns (updated sandbox, rowsAffected, error). rowsAffected=0 means the guard rejected the update.
	UpdateStatus(ctx context.Context, id string, status Status, allowedFrom ...Status) (Sandbox, int64, error)
	// AssignWorker sets worker_id and transitions status from scheduling→assigned atomically.
	AssignWorker(ctx context.Context, id string, workerID string) (Sandbox, error)
	// ClearWorker sets worker_id back to null; guarded to only act from cleanup_pending.
	ClearWorker(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	WithTx(tx pgx.Tx) Repository
}

type PostgresRepository struct {
	db pgxdb.DBTX
}

func NewPostgresRepository(db pgxdb.DBTX) *PostgresRepository {
	return &PostgresRepository{db: db}
}

func (r *PostgresRepository) WithTx(tx pgx.Tx) Repository {
	return &PostgresRepository{db: tx}
}

func (r *PostgresRepository) Create(ctx context.Context, s Sandbox) (Sandbox, error) {
	const q = `
		INSERT INTO sandboxes (id, runtime, status, image, port, ttl, preview_url, created_at, worker_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id`

	row := r.db.QueryRow(ctx, q,
		s.ID, s.Runtime, string(s.Status), s.Image, s.Port, s.TTL, s.PreviewURL, s.CreatedAt, s.WorkerID,
	)
	return scanSandbox(row)
}

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (Sandbox, error) {
	const q = `
		SELECT id, runtime, status, ttl, created_at, port, preview_url, image, worker_id
		FROM sandboxes WHERE id = $1 LIMIT 1`

	row := r.db.QueryRow(ctx, q, id)
	s, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, ErrNotFound
		}
		return Sandbox{}, err
	}
	return s, nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Sandbox, error) {
	const q = `
		SELECT id, runtime, status, ttl, created_at, port, preview_url, image, worker_id
		FROM sandboxes ORDER BY created_at DESC`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Sandbox
	for rows.Next() {
		s, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) UpdateStatus(ctx context.Context, id string, status Status, allowedFrom ...Status) (Sandbox, int64, error) {
	if len(allowedFrom) == 0 {
		return Sandbox{}, 0, errors.New("UpdateStatus: at least one allowedFrom status required")
	}

	placeholders := make([]string, len(allowedFrom))
	args := make([]any, 0, len(allowedFrom)+2)
	args = append(args, string(status), id)
	for i, st := range allowedFrom {
		placeholders[i] = "$" + strconv.Itoa(i+3)
		args = append(args, string(st))
	}

	q := `UPDATE sandboxes SET status = $1 WHERE id = $2 AND status IN (` +
		joinStrings(placeholders) +
		`) RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id`

	row := r.db.QueryRow(ctx, q, args...)
	s, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, 0, nil
		}
		return Sandbox{}, 0, err
	}
	return s, 1, nil
}

func (r *PostgresRepository) AssignWorker(ctx context.Context, id string, workerID string) (Sandbox, error) {
	const q = `
		UPDATE sandboxes SET worker_id = $1, status = 'assigned'
		WHERE id = $2 AND status = 'scheduling'
		RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id`

	row := r.db.QueryRow(ctx, q, workerID, id)
	s, err := scanSandbox(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, nil
		}
		return Sandbox{}, err
	}
	return s, nil
}

func (r *PostgresRepository) ClearWorker(ctx context.Context, id string) error {
	const q = `UPDATE sandboxes SET worker_id = NULL WHERE id = $1 AND status = 'cleanup_pending'`
	_, err := r.db.Exec(ctx, q, id)
	return err
}

func (r *PostgresRepository) Delete(ctx context.Context, id string) error {
	const q = `DELETE FROM sandboxes WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id)
	return err
}

// scanSandbox scans a row from a SELECT over all 9 sandbox columns.
type scanner interface {
	Scan(dest ...any) error
}

func scanSandbox(row scanner) (Sandbox, error) {
	var s Sandbox
	var createdAt time.Time
	var workerID pgtype.Text
	err := row.Scan(
		&s.ID, &s.Runtime, &s.Status, &s.TTL,
		&createdAt, &s.Port, &s.PreviewURL, &s.Image,
		&workerID,
	)
	if err != nil {
		return Sandbox{}, err
	}
	s.CreatedAt = createdAt
	if workerID.Valid {
		v := workerID.String
		s.WorkerID = &v
	}
	return s, nil
}

func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
