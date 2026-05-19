# app package

Top-level HTTP application: Chi router wiring, config loading, and middleware setup.

## New(cfg, sql, riverClient)

Constructs the app. Wire-up order matters:
1. Creates `db.Queries` from the `*sql.DB`.
2. Creates `sandbox.PostgresRepository` → `sandbox.Service` (no Docker — orchestrator is DB-only).
3. Creates `orchestrator.HTTPHandlers` with the service, DB querier, and River client.
4. Mounts handlers under `/api/sandboxes`.

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

## Integration tests

Tests in this package use `//go:build integration` and spin up a real Postgres container via testcontainers. They also build a full River client against the test DB (`setupRiverClient`). The fake worker (`fakeWorkerServer`) uses `UpdateSandboxRunning` instead of `CreateSandbox` because the orchestrator pre-creates the record before the gRPC call arrives.
