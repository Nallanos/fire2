-- name: CreateBuild :one
INSERT INTO builds (id, repo, ref, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetBuild :one
SELECT * FROM builds
WHERE id = $1 LIMIT 1;

-- name: ListBuilds :many
SELECT * FROM builds
ORDER BY created_at DESC;

-- name: UpdateBuild :one
UPDATE builds
SET status = $2, updated_at = $3
WHERE id = $1
RETURNING *;

-- name: CreateDeployment :one
INSERT INTO deployments (id, build_id, status, image_tag, created_at, updated_at, port)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetDeployment :one
SELECT * FROM deployments
WHERE id = $1 LIMIT 1;

-- name: ListDeployments :many
SELECT * FROM deployments
ORDER BY created_at DESC;

-- name: UpdateDeployment :one
UPDATE deployments
SET status = $2, updated_at = $3, port = $4
WHERE id = $1
RETURNING *;

-- name: GetDeploymentByBuildID :one
SELECT * FROM deployments
WHERE build_id = $1 LIMIT 1;

