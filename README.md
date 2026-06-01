# fire2

A sandbox orchestration service. The API accepts sandbox creation requests, schedules them onto a worker fleet via a River job queue, and tracks lifecycle events from Docker in real time.

## Architecture

```
                        ┌─────────────────────────────────────┐
  HTTP client  ──POST──►│  API  (cmd/api)                      │
                        │  · Chi router                        │
                        │  · River job queue (Postgres-backed) │
                        │  · gRPC event receiver  :7001        │
                        └────────────┬────────────────────────-┘
                                     │ River job
                                     ▼
                             ┌───────────────┐
                             │  PostgreSQL   │
                             └───────────────┘
                                     │ gRPC CreateSandbox
                                     ▼
                        ┌────────────────────────┐
                        │  Worker  (cmd/worker)   │
                        │  · Docker daemon        │
                        │  · Event reporter       │
                        └────────────────────────┘
```

**Flow:**
1. `POST /api/sandboxes` creates a sandbox row (`pending`) and enqueues a River job atomically in one transaction.
2. The River worker drives the sandbox through the state machine: `pending → scheduling → assigned → starting → running`.
3. The worker streams Docker events back to the API's gRPC server, which updates sandbox status in real time.
4. If all job attempts are exhausted a cleanup job removes the container and marks the sandbox `failed`.

## Sandbox status

| Status | Meaning |
|--------|---------|
| `pending` | Job enqueued, not yet scheduled |
| `scheduling` | Selecting a worker |
| `assigned` | Worker chosen, gRPC call in flight |
| `starting` | Container created, waiting for Docker start event |
| `running` | Container is live |
| `cleanup_pending` | All retries exhausted, cleanup job running |
| `failed` | Terminal failure |

## Quick start

```bash
# 1. Start Postgres
make sandbox-up

# 2. Apply migrations
make sandbox-migrate

# 3. Start API + 2 workers (auto-registers them)
make sandbox-start

# 4. Create sandboxes and verify
make sandbox-seed
```

Logs are written to `.sandbox/`.

## Manual setup

### Prerequisites

- Go 1.23+
- Docker
- [dbmate](https://github.com/amacneil/dbmate) for migrations
- A running PostgreSQL instance

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgresql://temporal:temporal@localhost/temporal` | Postgres connection string |
| `PORT` | `8080` | API HTTP port |
| `ORCHESTRATOR_GRPC_PORT` | `7001` | gRPC event receiver port (API side) |
| `WORKER_PORT` | `50051` | Worker gRPC port |
| `WORKER_ID` | hostname | Stable worker identity |
| `WORKER_ADVERTISED_HOST` | auto-detected | IP the API uses to reach this worker |
| `ORCHESTRATOR_GRPC_ADDR` | `127.0.0.1:7001` | Address of the API's gRPC receiver (worker side) |

### Steps

```bash
# 1. Export database URL
export DATABASE_URL=postgresql://temporal:temporal@localhost/temporal

# 2. Apply migrations (River tables are created automatically at API startup)
make sandbox-migrate

# 3. Start the API
make run

# 4. Start a worker (Docker must be available)
make run-worker

# 5. Register the worker
go run ./cmd/create_worker --port 50051 --cpu-budget 4 --mem-budget 8192
```

## API

### `GET /health`
Returns `ok`.

### `POST /api/sandboxes`
Creates a sandbox. Blocks until the container is running or the request times out (~45 s).

**Request body:**
```json
{
  "runtime":     "node",
  "image":       "node:20-alpine",
  "port":        10000,
  "ttl":         3600,
  "preview_url": "https://example.invalid"
}
```

`image`, `port`, `ttl`, and `preview_url` are optional. Default images per runtime:

| Runtime | Default image |
|---------|---------------|
| `node` / `nodejs` | `node:20-alpine` |
| `python` / `py` | `python:3.12-alpine` |
| `go` / `golang` | `golang:1.23-alpine` |

**Response** `201 Created`:
```json
{
  "id":          "abc123",
  "runtime":     "node",
  "status":      "running",
  "image":       "node:20-alpine",
  "port":        10000,
  "ttl":         3600,
  "preview_url": "",
  "worker_id":   "worker-1",
  "created_at":  "2026-06-01T10:00:00Z"
}
```

### `GET /api/sandboxes`
Lists all sandboxes ordered by creation time descending.

### `GET /api/sandboxes/{id}`
Returns a single sandbox by ID. `404` if not found.

## Development

### Run tests

```bash
# Unit tests
make test

# Integration tests (requires Docker + Postgres)
go test -tags=integration ./...
```

### Regenerate protobuf

```bash
make proto
```

### Smoke test a running stack

```bash
make sandbox-smoke
```

## Worker deployment (Ansible)

An Ansible playbook is available to deploy workers to remote VMs. See [`feat/ansible-worker-deploy`](../../tree/feat/ansible-worker-deploy) for the playbook and inventory configuration.
