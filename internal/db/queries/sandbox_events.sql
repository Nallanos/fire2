-- name: CreateSandboxEvent :one
INSERT INTO sandbox_events (
    id,
    sandbox_id,
    container_id,
    worker_id,
    event_type,
    action,
    actor_id,
    attributes,
    occurred_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6,
    $7,
    $8,
    $9
)
RETURNING *;
