package sandbox

import (
	"context"
	"errors"
	"fmt"
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
	// ListByUserID returns only sandboxes owned by userID, newest first.
	ListByUserID(ctx context.Context, userID string) ([]Sandbox, error)
	// UpdateStatus advances status only when current status is one of allowedFrom.
	// Returns (updated sandbox, rowsAffected, error). rowsAffected=0 means the guard rejected the update.
	UpdateStatus(ctx context.Context, id string, status Status, allowedFrom ...Status) (Sandbox, int64, error)
	// UpdateStatusFenced is UpdateStatus restricted to a sandbox hosted by workerID
	// whose current fencing token is token. A stale worker incarnation matches no
	// row, so it cannot move a sandbox's status.
	UpdateStatusFenced(ctx context.Context, id, workerID string, token int64, status Status, allowedFrom ...Status) (int64, error)
	// ClaimTTLExpired moves every non-terminal sandbox whose created_at + ttl
	// (seconds; <= 0 expires immediately) has passed to cleanup_pending and
	// returns their IDs. Rows locked by a concurrent claim are skipped.
	ClaimTTLExpired(ctx context.Context) ([]string, error)
	// ClaimLivenessExpired moves every active sandbox hosted by a worker that has
	// not listed it for longer than timeout to cleanup_pending and returns their IDs.
	ClaimLivenessExpired(ctx context.Context, timeout time.Duration) ([]string, error)
	// ClaimStalledCreation moves every sandbox that has been pending or scheduling
	// for longer than timeout to cleanup_pending and returns their IDs. Those
	// have no worker yet, so liveness never sees them; a create job that gave up
	// without leaving a trace would hold them until their TTL. timeout must be
	// positive.
	ClaimStalledCreation(ctx context.Context, timeout time.Duration) ([]string, error)
	// ReconcileInventory matches a worker's complete heartbeat inventory
	// against the sandboxes assigned to it, reacting to each reported state
	// (see Inventory): live ones get last_seen_at refreshed, running ones move
	// starting→running, dead ones are moved to cleanup_pending and returned as
	// dead. Unknown ones are left untouched and age toward liveness expiry.
	//
	// On the first inventory of a new incarnation (firstInventory), every active
	// sandbox that is not listed and was last seen before the worker registered
	// belonged to the previous process, whose VMs died with it: they are moved
	// to cleanup_pending and returned as lost. Sandboxes assigned since the
	// registration are spared (their create call may not have reached the worker).
	ReconcileInventory(ctx context.Context, workerID string, firstInventory bool, inv Inventory) (ReconcileResult, error)
	// AssignWorker sets worker_id and transitions status from scheduling→assigned atomically,
	// only if the worker still has capacity (otherwise ErrWorkerFull).
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
		INSERT INTO sandboxes (id, runtime, status, image, port, ttl, preview_url, created_at, worker_id, vcpu_count, mem_size_mib, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id`

	row := r.db.QueryRow(ctx, q,
		s.ID, s.Runtime, string(s.Status), s.Image, s.Port, s.TTL, s.PreviewURL, s.CreatedAt, s.WorkerID,
		s.VcpuCount, s.MemSizeMib, s.UserID,
	)
	return scanSandbox(row)
}

func (r *PostgresRepository) GetByID(ctx context.Context, id string) (Sandbox, error) {
	const q = `
		SELECT id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id
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
		SELECT id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id
		FROM sandboxes ORDER BY created_at DESC`
	return r.queryList(ctx, q)
}

func (r *PostgresRepository) ListByUserID(ctx context.Context, userID string) ([]Sandbox, error) {
	const q = `
		SELECT id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id
		FROM sandboxes WHERE user_id = $1 ORDER BY created_at DESC`
	return r.queryList(ctx, q, userID)
}

func (r *PostgresRepository) queryList(ctx context.Context, q string, args ...any) ([]Sandbox, error) {
	rows, err := r.db.Query(ctx, q, args...)
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
		`) RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id`

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

// AssignWorker only assigns when the worker still has room. It runs in a
// transaction: the worker row is locked first so concurrent assignments to the
// same worker are serialized, and the occupancy count is then read by a second
// statement (fresh snapshot) that sees every assignment committed before the
// lock was granted. A single UPDATE with a subquery count would not — its
// snapshot is taken before it waits on the lock.
//
// Returns ErrWorkerFull when the worker is at capacity, and a zero Sandbox with
// a nil error when the status guard rejected the update (advanced concurrently).
func (r *PostgresRepository) AssignWorker(ctx context.Context, id string, workerID string) (Sandbox, error) {
	beginner, ok := r.db.(interface {
		Begin(ctx context.Context) (pgx.Tx, error)
	})
	if !ok {
		return Sandbox{}, errors.New("AssignWorker: db cannot begin a transaction")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return Sandbox{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var capacity int
	err = tx.QueryRow(ctx, `SELECT capacity FROM worker WHERE id = $1 FOR UPDATE`, workerID).Scan(&capacity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, fmt.Errorf("assign worker: worker %s not found", workerID)
		}
		return Sandbox{}, err
	}

	// cleanup_pending still holds its VM until the cleanup job clears worker_id.
	const q = `
		UPDATE sandboxes SET worker_id = $1, status = 'assigned', last_seen_at = now()
		WHERE id = $2 AND status = 'scheduling'
		  AND (SELECT count(*) FROM sandboxes
		       WHERE worker_id = $1
		         AND status IN ('assigned', 'starting', 'running', 'cleanup_pending')) < $3
		RETURNING id, runtime, status, ttl, created_at, port, preview_url, image, worker_id, vcpu_count, mem_size_mib, user_id`

	s, err := scanSandbox(tx.QueryRow(ctx, q, workerID, id, capacity))
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return Sandbox{}, err
		}
		// No row updated: either the worker is full or the status guard rejected us.
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM sandboxes WHERE id = $1`, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Sandbox{}, ErrNotFound
			}
			return Sandbox{}, err
		}
		if Status(status) == StatusScheduling {
			return Sandbox{}, ErrWorkerFull
		}
		return Sandbox{}, nil
	}

	if err := tx.Commit(ctx); err != nil {
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

// scanSandbox scans a row from a SELECT over all 12 sandbox columns.
type scanner interface {
	Scan(dest ...any) error
}

func scanSandbox(row scanner) (Sandbox, error) {
	var s Sandbox
	var createdAt time.Time
	var workerID pgtype.Text
	var userID pgtype.Text
	err := row.Scan(
		&s.ID, &s.Runtime, &s.Status, &s.TTL,
		&createdAt, &s.Port, &s.PreviewURL, &s.Image,
		&workerID, &s.VcpuCount, &s.MemSizeMib, &userID,
	)
	if err != nil {
		return Sandbox{}, err
	}
	s.CreatedAt = createdAt
	if workerID.Valid {
		v := workerID.String
		s.WorkerID = &v
	}
	if userID.Valid {
		v := userID.String
		s.UserID = &v
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
