# app package

Top-level HTTP application: Chi router wiring, config loading, and middleware setup.

## New(cfg, pool, riverClient)

Constructs the app. Wire-up order matters:
1. Creates `sandbox.PostgresRepository`, `worker.PostgresRepository`, `auth.PostgresRepository` — all pgx-backed, straight off the `*pgxpool.Pool`. No `db.Queries`/sqlc layer — that generated package (`internal/db`) is currently absent from the tree and nothing imports it.
2. Creates `orchestrator.HTTPHandlers` with the pool, sandbox/worker repos, and River client.
3. Creates `auth.Service` and mounts auth handlers under `/api/auth` (unauthenticated) and sandbox handlers under `/api/sandboxes` (behind `orchestrator.RequireAuth`).

The `*river.Client[pgx.Tx]` is required — it is passed through to `HTTPHandlers` for the `createSandbox` River job flow.

## Middleware stack (applied globally)

`RequestID` → `RealIP` → `Logger` → `Recoverer`

## Config

Loaded from environment in `ConfigFromEnv()`:

| Var | Default | Field |
|-----|---------|-------|
| `PORT` | `8080` | `Port` |
| `DATABASE_URL` | `postgresql://temporal:temporal@localhost/temporal` | `DatabaseURL` |
| `ORCHESTRATOR_GRPC_PORT` | `7001` | `OrchestratorGRPCPort` |
| `ORCHESTRATOR_GRPC_BIND_HOST` | auto-discovered Tailscale IP (see `internal/packages/tailscale`), falls back to `""` (all interfaces) | `OrchestratorGRPCBindHost` |
| `HEARTBEAT_TIMEOUT` | `15s` | `HeartbeatTimeout` |
| `REAPER_INTERVAL` | `10s` | `ReaperInterval` |

## Integration tests

Tests in this package use `//go:build integration` and spin up a real Postgres container via testcontainers (`setupPostgres`/`setupPostgresWithPool`). They also build a full River client against the test DB (`setupFastRiverClient`, 50ms retry backoff instead of `StrongRetryPolicy`'s). `startFakeWorker`/`fakeWorkerGRPCServer` fakes the worker's gRPC server for tests that need a real `CreateSandbox` round-trip without a real Firecracker host.
