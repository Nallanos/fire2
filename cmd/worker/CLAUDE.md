# cmd/worker

Entry point for a worker node. Runs the `WorkerService` gRPC server and starts the Docker event reporter.

## What it does

1. Connects to the orchestrator over gRPC (`ORCHESTRATOR_GRPC_ADDR`).
2. Connects to the Docker daemon.
3. Creates `WorkerService` (capacity-aware, owns container lifecycle).
4. Starts `EventReporter` in a goroutine — watches Docker events, forwards them to the orchestrator via `OrchestratorService.IngestSandboxEvent`.
5. Serves `WorkerService` gRPC. With `WORKER_PORT` unset or `0` it binds an OS-assigned ephemeral port, reads the actual port back from the listener (`SetListenPort`), and reports it to the orchestrator via heartbeat. A non-zero `WORKER_PORT` pins a specific port.

The worker holds **no database credentials**. Heartbeats are sent to the orchestrator via `OrchestratorService.ReportWorkerHeartbeat`; the orchestrator upserts the worker row. The first heartbeat acts as self-registration.

## Environment variables

| Var | Default |
|-----|---------|
| `WORKER_PORT` | `0` (ephemeral, OS-assigned) |
| `ORCHESTRATOR_GRPC_ADDR` | `127.0.0.1:7001` |
| `WORKER_ID` | hostname |
| `WORKER_ADVERTISED_HOST` | auto-detected |
| `WORKER_HEARTBEAT_INTERVAL` | `5s` |
