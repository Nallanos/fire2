-- name: CreateWorker :one
INSERT INTO worker (id, status, address, Capacity, port, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetWorker :one
SELECT * FROM worker
WHERE id = $1 LIMIT 1;

-- name: ListWorkers :many
SELECT * FROM worker
ORDER BY id DESC;

-- name: UpdateWorker :one
UPDATE worker
SET status = $2, address = $3, Capacity = $4, port = $5
WHERE id = $1
RETURNING *;

-- name: DeleteWorker :exec
DELETE FROM worker
WHERE id = $1;