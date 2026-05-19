-- name: CreateWorker :one
INSERT INTO worker (id, status, address, Capacity, port, cpu_budget, mem_budget, cpu_usage, mem_usage, last_heartbeat, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: GetWorker :one
SELECT * FROM worker
WHERE id = $1 LIMIT 1;

-- name: ListWorkers :many
SELECT * FROM worker
ORDER BY id DESC;

-- name: UpdateWorker :one
UPDATE worker
SET status = $2, address = $3, Capacity = $4, port = $5, cpu_budget = $6, mem_budget = $7, cpu_usage = $8, mem_usage = $9, last_heartbeat = $10
WHERE id = $1
RETURNING *;

-- name: DeleteWorker :exec
DELETE FROM worker
WHERE id = $1;