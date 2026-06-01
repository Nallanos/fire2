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

### Running tests

```bash
# Unit tests only (no external dependencies)
go test ./...

# Unit tests for a specific package
go test ./internal/packages/orchestrator/...

# Integration tests (require Docker — spins up a real Postgres container)
go test -tags integration ./...

# Integration tests for a specific package, with verbose output
go test -tags integration ./internal/app/... -v

# Run a specific test by name
go test -tags integration ./internal/app/... -run TestRiverRetry_SuccessAfterTransientFailures -v
```

### DB migrations

```bash
dbmate --migrations-dir internal/db/migrations up
```

SQL query changes require regenerating sqlc code:

```bash
sqlc generate
```

## Testing

### Test types

| Type | Build tag | Dependencies | Command |
|------|-----------|--------------|---------|
| Unit | none | none | `go test ./...` |
| Integration | `integration` | Docker (for testcontainers Postgres) | `go test -tags integration ./...` |

### Test files

| File | Type | What it covers |
|------|------|----------------|
| `internal/packages/orchestrator/retry_policy_test.go` | Unit | `StrongRetryPolicy` backoff math — sequence (2s→4s→8s→16s→30s) and 30s cap |
| `internal/packages/orchestrator/jobs_test.go` | Unit | `CreateSandboxWorker.Work`: abandoned-sandbox guard, empty worker pool, DB error on ListWorkers |
| `internal/packages/orchestrator/scheduler_test.go` | Unit | Scheduler weighted-random distribution, edge cases (single worker, all at capacity, empty list) |
| `internal/packages/worker/heartbeat_test.go` | Unit | Worker heartbeat logic |
| `internal/app/app_integration_test.go` | Integration | Full sandbox API flow (POST → River job → fake gRPC worker → 201); shared test helpers |
| `internal/app/sandbox_flow_integration_test.go` | Integration | Multi-worker scheduling with real Docker daemon and event ingestion |
| `internal/app/river_retry_integration_test.go` | Integration | River retry scenarios: transient failures, exhausted retries, abandoned-sandbox guard, handler timeout |
| `test/minimal_config_test.go` | Unit | Minimal config smoke test with fake gRPC server |

### Integration test setup

Integration tests use **testcontainers** to spin up a real Postgres container. Docker must be running. Each test function creates its own isolated DB (no shared state between tests).

Key shared helpers live in `internal/app/app_integration_test.go`:
- `setupPostgresWithPool(t, ctx)` — starts Postgres container, returns `*sql.DB` + `*pgxpool.Pool`
- `setupRiverClient(t, ctx, pool, queries)` — applies River migrations, registers workers, starts River client
- `applyMigrations(sqlDB)` — runs all dbmate migrations from `internal/db/migrations/`
- `startFakeWorker(t, queries)` — starts a gRPC server implementing `WorkerServiceServer` that calls `UpdateSandboxRunning` on success

### River retry test helpers (`river_retry_integration_test.go`)

- `setupFastRiverClient` — like `setupRiverClient` but uses `fastRetryPolicy` (50ms fixed backoff) and `FetchCooldown: 1ms` / `FetchPollInterval: 1ms` so retry cycles complete in milliseconds
- `controllableWorkerServer{failN: N}` — gRPC worker that fails the first N calls then succeeds; `CallCount()` returns total calls made
- `slowWorkerServer{delay: D}` — gRPC worker that sleeps for duration D then returns an error; used to trigger the handler's 45s timeout path
- `Config.SandboxWaitTimeout` — overrides the handler's default 45s wait; set to a short value (e.g. 500ms) in tests that exercise the timeout branch

### Writing new tests

**Unit tests** (no build tag needed): place in `*_test.go` in the relevant package. Use `stubQuerier` from `internal/packages/orchestrator/jobs_test.go` as a pattern for faking `db.Querier` without a real DB.

**Integration tests**: add `//go:build integration` at the top and place in `internal/app/`. Reuse `setupPostgresWithPool` and `applyMigrations` from `app_integration_test.go`. Each test must be self-contained — register its own workers and use its own River client.

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
- Sandbox creation is asynchronous internally: the HTTP handler enqueues a River job and waits up to 45 seconds for completion, giving the caller a synchronous 201/502 experience.
