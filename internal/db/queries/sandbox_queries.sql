-- name: CreateSandbox :one
INSERT INTO sandboxes (id,  runtime, status, image, port, ttl, preview_url, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetSandbox :one
SELECT * FROM sandboxes
WHERE id = $1 LIMIT 1;

-- name: ListSandboxes :many
SELECT * FROM sandboxes
ORDER BY id DESC;

-- name: UpdateSandbox :one
UPDATE sandboxes
SET status = $2
WHERE id = $1
RETURNING *;

-- name: UpdateSandboxRunning :one
UPDATE sandboxes
SET status = $2, port = $3, image = $4
WHERE id = $1
RETURNING *;

-- name: UpdateSandboxIfQueued :one
UPDATE sandboxes
SET status = $2, port = $3, image = $4
WHERE id = $1 AND status = 'queued'
RETURNING *;

-- name: DeleteSandbox :exec
DELETE FROM sandboxes
WHERE id = $1;