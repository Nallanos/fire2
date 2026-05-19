# cmd/api

Entry point for the orchestrator API server. Starts two network listeners and a River job queue.

## Startup sequence

1. `pgxpool.New` — primary connection pool (used by River driver).
2. `stdlib.OpenDBFromPool` — wraps pool as `*sql.DB` for sqlc queries.
3. `rivermigrate.Migrate` — applies River schema tables to the DB at startup (idempotent).
4. `river.NewClient` — registers `CreateSandboxWorker`, sets `MaxAttempts=5`, `RetryPolicy=StrongRetryPolicy`.
5. `riverClient.Start(ctx)` — begins background job processing.
6. `app.New(cfg, sqlDB, riverClient)` — builds Chi router.
7. HTTP server on `PORT` (default 8081).
8. Orchestrator gRPC server on `ORCHESTRATOR_GRPC_PORT` (default 7001) — receives sandbox events from workers.

## Shutdown

On `SIGINT`/`SIGTERM`:
- HTTP server: 10s graceful shutdown.
- River client: 15s drain (deferred, runs after HTTP shutdown).
- pgxpool and sqlDB closed via `defer`.

## Environment variables

| Var | Default |
|-----|---------|
| `PORT` | `8080` |
| `DATABASE_URL` | `postgresql://temporal:temporal@localhost/temporal` |
| `ORCHESTRATOR_GRPC_PORT` | `7001` |
