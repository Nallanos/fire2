package worker

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github/nallanos/fire2/internal/packages/pgxdb"
)

var ErrNotFound = errors.New("worker not found")

// Repository is the storage interface for worker rows (used by the orchestrator only).
type Repository interface {
	Create(ctx context.Context, w Worker) (Worker, error)
	Get(ctx context.Context, id string) (Worker, error)
	Update(ctx context.Context, w Worker) (Worker, error)
	List(ctx context.Context) ([]Worker, error)
}

type PostgresRepository struct {
	db pgxdb.DBTX
}

func NewPostgresRepository(db pgxdb.DBTX) *PostgresRepository {
	return &PostgresRepository{db: db}
}

const workerColumns = `id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat`

func scanWorker(row pgx.Row) (Worker, error) {
	var w Worker
	var cpuUsage, memUsage int
	err := row.Scan(
		&w.ID, &w.Status, &w.Address, &w.Capacity, &w.Port,
		&w.Budget.Cpu_budget, &w.Budget.Mem_budget,
		&cpuUsage, &memUsage,
		&w.Last_heartbeat,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Worker{}, ErrNotFound
		}
		return Worker{}, err
	}
	w.cpu_usage = cpuUsage
	w.mem_usage = memUsage
	return w, nil
}

func (r *PostgresRepository) Create(ctx context.Context, w Worker) (Worker, error) {
	const q = `
		INSERT INTO worker (id, status, address, capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING ` + workerColumns

	if w.Last_heartbeat.IsZero() {
		w.Last_heartbeat = time.Now().UTC()
	}
	row := r.db.QueryRow(ctx, q,
		w.ID, string(w.Status), w.Address, w.Capacity, w.Port,
		w.Budget.Cpu_budget, w.Budget.Mem_budget,
		w.cpu_usage, w.mem_usage, w.Last_heartbeat,
	)
	return scanWorker(row)
}

func (r *PostgresRepository) Get(ctx context.Context, id string) (Worker, error) {
	const q = `SELECT ` + workerColumns + ` FROM worker WHERE id = $1 LIMIT 1`
	return scanWorker(r.db.QueryRow(ctx, q, id))
}

func (r *PostgresRepository) Update(ctx context.Context, w Worker) (Worker, error) {
	const q = `
		UPDATE worker
		SET status = $2, address = $3, capacity = $4, port = $5,
		    cpu_budget = $6, mem_budget = $7, cpu_usage = $8, mem_usage = $9, last_heartbeat = $10
		WHERE id = $1
		RETURNING ` + workerColumns

	if w.Last_heartbeat.IsZero() {
		w.Last_heartbeat = time.Now().UTC()
	}
	row := r.db.QueryRow(ctx, q,
		w.ID, string(w.Status), w.Address, w.Capacity, w.Port,
		w.Budget.Cpu_budget, w.Budget.Mem_budget,
		w.cpu_usage, w.mem_usage, w.Last_heartbeat,
	)
	updated, err := scanWorker(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Worker{}, ErrNotFound
		}
		return Worker{}, err
	}
	return updated, nil
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
		var w Worker
		var cpuUsage, memUsage int
		if err := rows.Scan(
			&w.ID, &w.Status, &w.Address, &w.Capacity, &w.Port,
			&w.Budget.Cpu_budget, &w.Budget.Mem_budget,
			&cpuUsage, &memUsage,
			&w.Last_heartbeat,
		); err != nil {
			return nil, err
		}
		w.cpu_usage = cpuUsage
		w.mem_usage = memUsage
		workers = append(workers, w)
	}
	return workers, rows.Err()
}
