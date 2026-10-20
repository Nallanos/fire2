# worker package

Worker-side gRPC server, heartbeat logic, and forwarding of sandbox state changes. Sandboxes themselves are owned by `internal/packages/sandboxmgr`: the worker never talks to Firecracker.

## Key types

| Type | File | Role |
|------|------|------|
| `WorkerGRPCServer` | `grpc_client.go` | Implements `WorkerService` proto — thin adapter over `WorkerService` |
| `WorkerService` | `worker_service.go`, `sandbox.go` | Heartbeat, capacity computation; sandbox calls delegate to a `SandboxManager` |
| `SandboxManager` | `sandbox.go` | Interface over `sandboxmgr.Manager` (stubbed in tests) |
| `EventReporter` | `event_reporter.go` | Forwards the manager's state changes to the orchestrator |
| `Worker` | `model.go` | In-memory worker state (ID, status, budgets, running count) |

## WorkerService

`NewWorkerService(OrchestratorServiceClient, SandboxManager)` creates the service. It owns:
- **Capacity computation** — computed on every heartbeat (`computeCapacity`) and handed to the manager with `SetCapacity`, which enforces it.
- **Heartbeat** — registers once through `OrchestratorService.RegisterWorker` to receive a fencing token, then periodically reports CPU/memory usage through `ReportWorkerHeartbeat` with that token. An older incarnation cannot update the worker row. The inventory (`sandboxes`) is the manager's `Snapshot()`: every tracked sandbox with its last known state.
- **Sandbox lifecycle** — `CreateSandbox`/`StopSandbox`/`RemoveSandbox` delegate to the manager. `grpc_client.go` maps its errors: `ErrSandboxNotFound` → `NotFound`, `ErrBusy` → `Unavailable` (retried by the orchestrator), `ErrAtCapacity` → `ResourceExhausted`.
- **No DB credentials** — the worker holds no Postgres connection; all persistence goes through the orchestrator.

## EventReporter

`NewEventReporter(OrchestratorServiceClient, workerID, *WorkerService)`.

`Run(ctx)` reads the manager's `Events()` and sends each change through `OrchestratorService.IngestSandboxEvent` with the current fencing token. Best-effort: nothing is sent before registration, and a failed send is logged and dropped. The heartbeat inventory is the guarantee; events only make the orchestrator converge sooner.

## Heartbeat

`HeartbeatExpired(lastHeartbeat, timeout) bool` — returns true if the heartbeat is overdue (default timeout 15s) or if `lastHeartbeat` is zero. Used to detect stale workers.

## gRPC server

`ServeGRPC(address, server)` listens and serves the `WorkerService` proto, then starts the heartbeat loop once bound. Workers are identified by `WORKER_ID` (defaults to hostname).

## Constraints

- gRPC uses insecure credentials — intended for trusted private networks only (bind host is normally the auto-discovered Tailscale IP, see `internal/packages/tailscale`).
