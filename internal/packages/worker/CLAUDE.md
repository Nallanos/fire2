# worker package

Worker-side gRPC server, sandbox lifecycle management, Docker event reporting, and heartbeat logic.

## Key types

| Type | File | Role |
|------|------|------|
| `WorkerGRPCServer` | `main.go` | Implements `WorkerService` proto — thin adapter over `WorkerService` |
| `WorkerService` | `worker_service.go` | Business logic: capacity check, container lifecycle, heartbeat |
| `EventReporter` | `event_reporter.go` | Watches Docker events, streams them to orchestrator gRPC |
| `Worker` | `model.go` | In-memory worker state (ID, status, budgets, running count) |

## WorkerService

`NewWorkerService(dockerClient, db.Querier)` creates the service. It owns:
- **Capacity enforcement** — `running_sandboxes` is incremented before `CreateAndStart` and decremented on failure.
- **Heartbeat** — periodically reads CPU/memory usage (`gopsutil`) and writes to DB via `UpdateWorker`.
- **Container lifecycle** — delegates to `sandbox.Service` (with Docker client).

`Worker` struct holds current state (protected by a `sync.Mutex` on `running_sandboxes`).

## EventReporter

`NewEventReporter(dockerClient, OrchestratorServiceClient, workerID)`.

`Run(ctx)` is a blocking loop:
- Streams container events from Docker daemon.
- Converts each event to `SandboxEvent` proto (reads `sandbox_id` from container labels).
- Calls `OrchestratorService.IngestSandboxEvent` for each event.
- Reconnects automatically (`sandboxEventRetryDelay = 2s`) if the Docker event stream drops.

## Heartbeat

`HeartbeatExpired(lastHeartbeat, timeout) bool` — returns true if the heartbeat is overdue (default timeout 15s) or if `lastHeartbeat` is zero. Used to detect stale workers.

## gRPC server

`ServeGRPC(address, server)` listens and serves the `WorkerService` proto. Workers are identified by the hostname of the machine they run on (used as `workerID` for event reporting).

## Constraints

- No auto-registration: the worker must be registered in the DB via `create_worker` CLI before the scheduler can route to it.
- Docker labels `sandbox_id` and `id` are set on containers so `EventReporter` can correlate events back to sandboxes.
- gRPC uses insecure credentials — intended for trusted private networks only.
