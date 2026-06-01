# cmd/worker

Entry point for a worker node. Runs the `WorkerService` gRPC server and starts the Docker event reporter.

## What it does

1. Connects to Postgres and Docker daemon.
2. Creates `WorkerService` (capacity-aware, owns container lifecycle).
3. Starts `EventReporter` in a goroutine — watches Docker events, forwards them to the orchestrator via `OrchestratorService.IngestSandboxEvent`.
4. Serves `WorkerService` gRPC on `WORKER_PORT`.

## Environment variables

| Var | Default |
|-----|---------|
| `DATABASE_URL` | `postgresql://temporal:temporal@localhost/temporal` |
| `WORKER_PORT` | `50051` |
| `ORCHESTRATOR_GRPC_ADDR` | `127.0.0.1:7001` |

## Notes

- The worker ID used for event reporting is the machine **hostname** (`os.Hostname()`).
- Workers must be registered in the DB via `create_worker` before the scheduler routes traffic to them. This binary does not self-register.
- If the event client fails to connect at startup, the server still runs but events won't be reported (logged, not fatal).
