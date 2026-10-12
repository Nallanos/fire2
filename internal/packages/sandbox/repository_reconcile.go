package sandbox

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// InventoryItem is one sandbox a worker reports in its heartbeat inventory.
type InventoryItem struct {
	SandboxID string
	State     string
}

func (r *PostgresRepository) UpdateStatusFenced(ctx context.Context, id, workerID string, token int64, status Status, allowedFrom ...Status) (int64, error) {
	if len(allowedFrom) == 0 {
		return 0, errors.New("UpdateStatusFenced: at least one allowedFrom status required")
	}

	placeholders := make([]string, len(allowedFrom))
	args := []any{string(status), id, workerID, token}
	for i, st := range allowedFrom {
		placeholders[i] = "$" + strconv.Itoa(i+5)
		args = append(args, string(st))
	}

	q := `UPDATE sandboxes s SET status = $1
		FROM worker w
		WHERE s.id = $2 AND s.worker_id = w.id AND w.id = $3 AND w.fencing_token = $4
		  AND s.status IN (` + strings.Join(placeholders, ", ") + `)`
	tag, err := r.db.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

const claimToCleanupPrefix = `
	UPDATE sandboxes SET status = 'cleanup_pending'
	WHERE id IN (SELECT id FROM sandboxes WHERE `

const claimToCleanupSuffix = ` ORDER BY created_at FOR UPDATE SKIP LOCKED)
	RETURNING id`

func (r *PostgresRepository) ClaimTTLExpired(ctx context.Context) ([]string, error) {
	const q = claimToCleanupPrefix + `
		status IN ('pending', 'scheduling', 'assigned', 'starting', 'running')
		AND created_at + make_interval(secs => LEAST(GREATEST(ttl, 0), 3153600000)::double precision) <= now()` + claimToCleanupSuffix
	return r.queryIDs(ctx, q)
}

func (r *PostgresRepository) ClaimLivenessExpired(ctx context.Context, timeout time.Duration) ([]string, error) {
	const q = claimToCleanupPrefix + `
		status IN ('assigned', 'starting', 'running')
		AND worker_id IS NOT NULL
		AND COALESCE(last_seen_at, created_at) < now() - make_interval(secs => $1)` + claimToCleanupSuffix
	return r.queryIDs(ctx, q, timeout.Seconds())
}

func (r *PostgresRepository) ReconcileInventory(ctx context.Context, workerID string, firstInventory bool, items []InventoryItem) ([]string, error) {
	ids := make([]string, 0, len(items))
	var running []string
	for _, it := range items {
		ids = append(ids, it.SandboxID)
		if strings.EqualFold(strings.TrimSpace(it.State), "running") {
			running = append(running, it.SandboxID)
		}
	}

	if firstInventory {
		const q = `UPDATE sandboxes SET last_seen_at = now()
			WHERE worker_id = $1 AND status IN ('assigned', 'starting', 'running')`
		if _, err := r.db.Exec(ctx, q, workerID); err != nil {
			return nil, err
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	const seen = `UPDATE sandboxes SET last_seen_at = now()
		WHERE worker_id = $1 AND id = ANY($2) AND status IN ('assigned', 'starting', 'running')`
	if _, err := r.db.Exec(ctx, seen, workerID, ids); err != nil {
		return nil, err
	}

	if len(running) > 0 {
		const advance = `UPDATE sandboxes SET status = 'running'
			WHERE worker_id = $1 AND id = ANY($2) AND status = 'starting'`
		if _, err := r.db.Exec(ctx, advance, workerID, running); err != nil {
			return nil, err
		}
	}

	const orphans = `
		SELECT x FROM unnest($2::text[]) AS x
		WHERE NOT EXISTS (
			SELECT 1 FROM sandboxes s
			WHERE s.id = x AND s.worker_id = $1 AND s.status NOT IN ('failed', 'stopped', 'cleaned_up')
		)`
	return r.queryIDs(ctx, orphans, workerID, ids)
}

func (r *PostgresRepository) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
