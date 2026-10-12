# worker package

Worker-side gRPC server, Firecracker sandbox lifecycle management, instance event reporting, and heartbeat logic.

## Key types

| Type | File | Role |
|------|------|------|
| `WorkerGRPCServer` | `grpc_client.go` | Implements `WorkerService` proto — thin adapter over `WorkerService` |
| `WorkerService` | `worker_service.go` | Business logic: capacity check, sandbox lifecycle, heartbeat |
| `EventReporter` | `event_reporter.go` | Polls running sandboxes' instance info, streams it to orchestrator gRPC |
| `Worker` | `model.go` | In-memory worker state (ID, status, budgets, running count) |

## WorkerService

`NewWorkerService(OrchestratorServiceClient)` creates the service. It owns:
- **Capacity enforcement** — `running_sandboxes` is incremented before `CreateSandbox` and decremented on failure.
- **Heartbeat** — periodically reads CPU/memory usage (`gopsutil`) and calls `OrchestratorService.ReportWorkerHeartbeat` via gRPC. The first heartbeat acts as self-registration on the orchestrator side.
- **Sandbox lifecycle** — delegates to `internal/packages/runtime` (Firecracker microVMs), tracked in `runningSandboxes`/`deletingSandboxes` maps.
- **No DB credentials** — the worker holds no Postgres connection; all persistence goes through the orchestrator.

`Worker` struct holds current state (protected by a `sync.Mutex` on `running_sandboxes`).

## EventReporter

`NewEventReporter(OrchestratorServiceClient, workerID, *WorkerService)`.

`Run(ctx)` is a blocking poll loop (Firecracker has no push-based event stream like Docker's):
- Every `sandboxEventPollInterval` (2s), samples each running sandbox's instance info via `RunningSandboxClients()`.
- Converts each into a `SandboxEvent` proto.
- Calls `OrchestratorService.IngestSandboxEvent` for each event. A single sandbox failing to report only logs and is skipped.

## Heartbeat

`HeartbeatExpired(lastHeartbeat, timeout) bool` — returns true if the heartbeat is overdue (default timeout 15s) or if `lastHeartbeat` is zero. Used to detect stale workers.

## gRPC server

`ServeGRPC(address, server)` listens and serves the `WorkerService` proto, then starts the heartbeat loop once bound. Workers are identified by `WORKER_ID` (defaults to hostname).

## Constraints

- gRPC uses insecure credentials — intended for trusted private networks only (bind host is normally the auto-discovered Tailscale IP, see `internal/packages/tailscale`).
