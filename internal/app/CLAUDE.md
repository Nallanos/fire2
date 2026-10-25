# app package

Top-level HTTP application: Chi router wiring, config loading, and middleware setup.

## New(cfg, pool, riverClient)

Constructs the app. Wire-up order matters:
1. Creates `sandbox.PostgresRepository`, `worker.PostgresRepository`, `auth.PostgresRepository` — all pgx-backed, straight off the `*pgxpool.Pool`. No `db.Queries`/sqlc layer — that generated package (`internal/db`) is currently absent from the tree and nothing imports it.
2. Creates `orchestrator.HTTPHandlers` with the pool, sandbox/worker repos, and River client.
3. Creates `auth.Service` and mounts auth handlers under `/api/auth` (unauthenticated) and sandbox handlers under `/api/sandboxes` (behind `orchestrator.RequireAuth`).

The `*river.Client[pgx.Tx]` is required — it is passed through to `HTTPHandlers` for the `createSandbox` River job flow.

## Orchestrator wiring (`orchestrator.go`)

Shared by `cmd/api` and the divergence harness (`test/divergence`), so the harness tests the wiring that runs in production:

- `NewRiverClient(pool, cfg, retry)` — the five River workers (create, cleanup, reap workers, reap sandbox, remove orphan), the three queues (`default`, `cleanup`, `reap_sandbox`) and the two periodic jobs, whose intervals and timeouts come from `cfg` (`ReaperInterval`, `HeartbeatTimeout`, `SandboxReaperInterval`, `SandboxLivenessTimeout`). The caller runs the River migrations and starts the client.
- `NewEventServer(pool, riverClient)` — the `EventGRPCServer` with `WithTransactions` and `WithRiverClient`.

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
| `SANDBOX_LIVENESS_TIMEOUT` | `15s` | `SandboxLivenessTimeout` |
| `SANDBOX_REAPER_INTERVAL` | `1m` | `SandboxReaperInterval` |

## Integration tests

Tests in this package use `//go:build integration` and spin up a real Postgres container via testcontainers (`setupPostgres`/`setupPostgresWithPool`). They also build a River client against the test DB (`setupFastRiverClient`: only the create and cleanup workers, 50ms retry backoff instead of `StrongRetryPolicy`'s). `startFakeWorker`/`fakeWorkerGRPCServer` fakes the worker's gRPC server for tests that need a real `CreateSandbox` round-trip without a real Firecracker host; it remembers the sandboxes it created and lists them in `ListRunningSandboxes`.

Every sandbox route sits behind `RequireAuth`: `testutil.AuthHeader(t, pool)` creates a user and a non-expiring session and returns the `Authorization: Bearer` value; `createSandbox`, `listSandboxes`, `getSandbox`, `postSandbox` and `doRequest` take it.
