# orchestrator package

Orchestrates sandbox creation across the worker fleet. Contains the HTTP handlers, gRPC dispatch, scheduler, event ingestion server, and River job definitions.

## Key files

| File | Responsibility |
|------|---------------|
| `http.go` | Chi HTTP handlers for `/api/sandboxes` (POST/GET) |
| `scheduler.go` | `Scheduler` — weighted-random worker selection |
| `grpc_client.go` | `Client` wrapping `WorkerService` gRPC; `CreateSandboxOnLeastUsedWorker` |
| `event_grpc.go` | `EventGRPCServer` (OrchestratorService) — ingests events from workers |
| `jobs.go` | River job: `CreateSandboxArgs`, `CreateSandboxWorker` |
| `retry_policy.go` | `StrongRetryPolicy` — exponential backoff for River jobs |

## Sandbox creation flow (POST /api/sandboxes)

1. Handler pre-creates a DB record (`status=queued`) with resolved image and port.
2. Subscribes to River job events (before enqueueing — avoids race).
3. Enqueues a `CreateSandboxArgs` River job.
4. Waits up to **45 seconds** on the subscription channel for completion.
5. `CreateSandboxWorker.Work()` calls `CreateSandboxOnLeastUsedWorker()` then calls `db.UpdateSandboxRunning()` on success.
6. Handler fetches the updated sandbox from DB and returns 201.
7. On `JobStateDiscarded` (all retries exhausted) or timeout, marks sandbox `failed` and returns 502.

## Scheduler

`ChooseLeastUsedWorker` uses **weighted-random** selection:
- Weight = `0.7 × CPU_available_ratio + 0.3 × memory_available_ratio`
- Prefers workers with `status=active`; falls back to any worker if none are active.
- Falls back to deterministic least-used if all weights are zero.

## Event ingestion (gRPC :7001)

`IngestSandboxEvent` receives Docker events from workers and:
- Persists them to `sandbox_events`.
- Translates Docker actions to sandbox statuses: `start→running`, `die/stop/kill/oom→failed`.

## Retry policy

`StrongRetryPolicy` — `min(2^attempt seconds, 30s)` backoff. `MaxAttempts=5`. Total worst-case: 60s.
River fires `EventKindJobFailed` for both retryable and discarded jobs — the handler checks `event.Job.State == JobStateDiscarded` to distinguish final failure.

## Dependencies on db.Querier

`CreateSandboxWorker` and `HTTPHandlers` both need `db.Querier` for:
- `ListWorkers` — select worker pool for scheduling
- `CreateSandbox` — pre-create record in handler
- `UpdateSandboxRunning` — set status/port/image after gRPC success
- `UpdateSandbox` — mark `failed` on timeout or job exhaustion
