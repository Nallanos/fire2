package worker

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

var ErrNotFound = errors.New("worker not found")

// ErrFenced means the presented fencing token is behind the current one: an
// older process of the worker, which must stay refused for good.
var ErrFenced = errors.New("worker fencing token is no longer current")

// ErrWorkerUnknown means the database has no such worker, or a token behind the
// one presented: the database is late (row deleted, restored from an older
// backup), not the process. The worker can register again.
var ErrWorkerUnknown = errors.New("worker is unknown to the database")

// Repository is the storage interface for worker rows (used by the orchestrator only).
type Repository interface {
	Create(ctx context.Context, w Worker) (Worker, error)
	Register(ctx context.Context, w Worker) (Worker, error)
	Get(ctx context.Context, id string) (Worker, error)
	Update(ctx context.Context, w Worker) (Worker, error)
	List(ctx context.Context) ([]Worker, error)
	ListActive(ctx context.Context) ([]Worker, error)
	MarkStaleInactive(ctx context.Context, timeout time.Duration) (int64, error)
	// ClaimInventory records that the incarnation identified by token has had its
	// first complete inventory reconciled. It returns true exactly once per
	// token, and false if the token is no longer current. Call it in the same
	// transaction as Update so the worker row stays locked.
	ClaimInventory(ctx context.Context, id string, token int64) (first bool, err error)
	WithTx(tx pgx.Tx) Repository
}

type PostgresRepository struct {
	db pgxdb.DBTX
}

func NewPostgresRepository(db pgxdb.DBTX) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const workerColumns = `id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, fencing_token`

// nullablePort maps the in-memory port (0 = not yet reported) to a value the
// driver writes as SQL NULL, keeping the hardcoded-default-port logic out of
// the database.
func nullablePort(port int) *int {
	if port <= 0 {
		return nil
	}
	return &port
}

func scanWorker(row pgx.Row) (Worker, error) {
	var w Worker
	var cpuUsage, memUsage int
	var port *int
	err := row.Scan(
		&w.ID, &w.Status, &w.Address, &w.Capacity, &port,
		&w.Budget.Cpu_budget, &w.Budget.Mem_budget,
		&cpuUsage, &memUsage,
		&w.Last_heartbeat, &w.FencingToken,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Worker{}, ErrNotFound
		}
		return Worker{}, err
	}
	if port != nil {
		w.Port = *port
	}
	w.cpu_usage = cpuUsage
	w.mem_usage = memUsage
	return w, nil
}

func (r *PostgresRepository) Create(ctx context.Context, w Worker) (Worker, error) {
	const q = `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, fencing_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING ` + workerColumns

	if w.Last_heartbeat.IsZero() {
		w.Last_heartbeat = time.Now().UTC()
	}
	row := r.db.QueryRow(ctx, q,
		w.ID, string(w.Status), w.Address, w.Capacity, nullablePort(w.Port),
		w.Budget.Cpu_budget, w.Budget.Mem_budget,
		w.cpu_usage, w.mem_usage, w.Last_heartbeat, w.FencingToken,
	)
	return scanWorker(row)
}

// Register atomically claims a new fencing token for this worker ID. A new
// process starts at token 1; each replacement advances the persisted token so
// heartbeats from an older process can no longer update the row.
//
// w.FencingToken is the last token the registering process held (0 for a fresh
// one). The new token is above both it and the persisted one, so a database that
// lost its token (restored from an older backup, row deleted) cannot hand out
// one that this worker, or an older process of it, ever held: an old process
// keeps being refused.
//
// last_heartbeat is the database's clock, like sandboxes.last_seen_at, which
// the sandbox reaper compares it with.
func (r *PostgresRepository) Register(ctx context.Context, w Worker) (Worker, error) {
	const q = `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, fencing_token)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), GREATEST($10::bigint, 0) + 1)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			address = EXCLUDED.address,
			capacity = EXCLUDED.capacity,
			port = EXCLUDED.port,
			cpu_budget = EXCLUDED.cpu_budget,
			mem_budget = EXCLUDED.mem_budget,
			cpu_usage = EXCLUDED.cpu_usage,
			mem_usage = EXCLUDED.mem_usage,
			last_heartbeat = now(),
			inactive_since = NULL,
			fencing_token = GREATEST(worker.fencing_token, $10::bigint) + 1,
			registered_at = now()
		RETURNING ` + workerColumns

	return scanWorker(r.db.QueryRow(ctx, q,
		w.ID, string(w.Status), w.Address, w.Capacity, nullablePort(w.Port),
		w.Budget.Cpu_budget, w.Budget.Mem_budget,
		w.cpu_usage, w.mem_usage, w.FencingToken,
	))
}

func (r *PostgresRepository) WithTx(tx pgx.Tx) Repository {
	return &PostgresRepository{db: tx}
}

func (r *PostgresRepository) ClaimInventory(ctx context.Context, id string, token int64) (bool, error) {
	const q = `UPDATE worker SET inventory_token = $2
		WHERE id = $1 AND fencing_token = $2 AND inventory_token <> $2`
	tag, err := r.db.Exec(ctx, q, id, token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Worker, error) {
	const q = `SELECT ` + workerColumns + ` FROM worker WHERE id = $1 LIMIT 1`
	return scanWorker(r.db.QueryRow(ctx, q, id))
}

// Update refreshes the worker row from a heartbeat, only if w.FencingToken is
// still the current one. When it is not, it tells the two refusals apart by
// reading the row again with the same handle (the heartbeat's transaction):
//   - ErrWorkerUnknown if the row is gone or its token is behind the one
//     presented: the database lost track of this worker, which can register again;
//   - ErrFenced otherwise: a newer process holds the row, this one is stale.
//
// last_heartbeat is set by the database, not taken from w: the sandbox reaper
// compares it with sandboxes.last_seen_at, written by the same clock. A
// heartbeat also ends a worker's inactivity.
func (r *PostgresRepository) Update(ctx context.Context, w Worker) (Worker, error) {
	const q = `
		UPDATE worker
		SET status = $2, address = $3, capacity = $4, port = $5,
		    cpu_budget = $6, mem_budget = $7, cpu_usage = $8, mem_usage = $9,
		    last_heartbeat = now(), inactive_since = NULL
		WHERE id = $1 AND fencing_token = $10
		RETURNING ` + workerColumns

	row := r.db.QueryRow(ctx, q,
		w.ID, string(w.Status), w.Address, w.Capacity, nullablePort(w.Port),
		w.Budget.Cpu_budget, w.Budget.Mem_budget,
		w.cpu_usage, w.mem_usage, w.FencingToken,
	)
	updated, err := scanWorker(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Worker{}, r.whyRefused(ctx, w.ID, w.FencingToken)
		}
		return Worker{}, err
	}
	return updated, nil
}

// whyRefused explains an Update that matched no row: ErrWorkerUnknown when the
// database is behind the presented token, ErrFenced when it is ahead.
func (r *PostgresRepository) whyRefused(ctx context.Context, id string, presented int64) error {
	var current int64
	err := r.db.QueryRow(ctx, `SELECT fencing_token FROM worker WHERE id = $1`, id).Scan(&current)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrWorkerUnknown
	case err != nil:
		return err
	case current < presented:
		return ErrWorkerUnknown
	}
	return ErrFenced
}

// MarkStaleInactive flips every active worker whose heartbeat is older than
// timeout to inactive, in a single atomic statement, and returns the number of
// workers reaped. inactive_since records when, so the sandbox reaper can tell a
// worker that is gone for good from one the orchestrator merely lost sight of.
// The WHERE guard (status = active AND last_heartbeat < cutoff)
// makes this safe against a worker that heartbeats back to life mid-sweep: such
// a worker no longer matches and is left active. A non-positive timeout falls
// back to the package default.
func (r *PostgresRepository) MarkStaleInactive(ctx context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		timeout = defaultHeartbeatTimeout
	}
	const q = `
		UPDATE worker
		SET status = $2, inactive_since = now()
		WHERE status = $3
		  AND last_heartbeat < now() - make_interval(secs => $1)`
	tag, err := r.db.Exec(ctx, q,
		timeout.Seconds(),
		string(WorkerStatusInactive),
		string(WorkerStatusActive),
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *PostgresRepository) List(ctx context.Context) ([]Worker, error) {
	const q = `SELECT ` + workerColumns + ` FROM worker ORDER BY id`
	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

func (r *PostgresRepository) ListActive(ctx context.Context) ([]Worker, error) {
	const q = `SELECT ` + workerColumns + ` FROM worker WHERE status = $1 ORDER BY id`
	rows, err := r.db.Query(ctx, q, string(WorkerStatusActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}
