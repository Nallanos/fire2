# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make test                    # Run all tests
make proto                   # Regenerate Go code from .proto files (protoc)
make run                     # Start the API server (HTTP :8081 + gRPC :7001)
make run-worker              # Start a worker server (gRPC :50051)
```

### Development sandbox (full local stack)

```bash
make sandbox-up              # Start PostgreSQL via docker-compose
make sandbox-migrate         # Apply DB migrations (dbmate)
make sandbox-start           # Start API + N workers in background
make sandbox-register-worker # Register a worker in the database
make sandbox-seed            # Create test sandboxes (Node, Python, Go)
make sandbox-smoke           # Smoke-test the API endpoints
```

### Running integration tests

Integration tests are gated behind the `integration` build tag and spin up a real Postgres container via testcontainers:

```bash
go test -tags integration ./...
```

### DB migrations

```bash
dbmate --migrations-dir internal/db/migrations up
```

SQL query changes require regenerating sqlc code:

```bash
sqlc generate
```

## Architecture

Fire2 is an orchestrator-worker system for managing sandbox containers. The **orchestrator** (`cmd/api`) exposes a REST API and schedules container workloads across a fleet of **workers** (`cmd/worker`), each of which drives a local Docker daemon.

### Services

| Binary | Ports | Purpose |
|--------|-------|---------|
| `cmd/api` | HTTP :8081, gRPC :7001 | REST API + event ingestion server |
| `cmd/worker` | gRPC :50051 | Container lifecycle manager |
| `cmd/create_worker` | — | CLI to register a worker in the DB |

### Request flow

1. `POST /api/sandboxes` → orchestrator HTTP handler
2. Scheduler queries DB for available workers, selects one via **weighted random** (70 % CPU available + 30 % memory available)
3. Orchestrator calls `WorkerService.CreateSandbox` (gRPC) on the chosen worker
4. Worker creates and starts the Docker container, returns metadata
5. Worker `EventReporter` watches Docker events and streams them back to the orchestrator via `OrchestratorService.IngestSandboxEvent` (gRPC)
6. Orchestrator persists events in the `sandbox_events` table

### Key internal packages

| Package | Path | Role |
|---------|------|------|
| `app` | `internal/app/` | Chi router, config, top-level HTTP handlers |
| `orchestrator` | `internal/packages/orchestrator/` | Scheduler, sandbox HTTP handlers, event gRPC client |
| `sandbox` | `internal/packages/sandbox/` | Sandbox domain model, service, repository |
| `worker` | `internal/packages/worker/` | Worker gRPC server, container service, event reporter, heartbeat |
| `docker` | `internal/packages/docker/` | Thin Docker client wrapper |
| `db` | `internal/db/` | sqlc-generated query code + migrations |

### gRPC contracts

- `proto/worker/v1/worker.proto` — `WorkerService`: `CreateSandbox`, `StopSandbox`, `RemoveSandbox`, `GetWorkerInfo`
- `proto/orchestrator/v1/orchestrator.proto` — `OrchestratorService`: `IngestSandboxEvent`

Generated Go code lives in `gen/`.

### Data persistence

PostgreSQL (via pgx/v5 + sqlc). Schema managed with dbmate migrations in `internal/db/migrations/`. Workers must be explicitly registered with `create_worker` before the scheduler can route to them. Workers periodically heartbeat resource usage (CPU/mem) back to the DB.

### Notable constraints

- gRPC connections use insecure credentials — intended for private/local networks only.
- Workers are statically registered; there is no auto-discovery.
- The `river` job-queue dependency is present in `go.mod` but not yet wired up.
