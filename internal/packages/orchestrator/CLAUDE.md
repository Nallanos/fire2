# orchestrator package

## Purpose

Coordinates sandbox lifecycle across the API, River job queue, Docker workers, and the event stream. Owns the create-sandbox HTTP handler, both River job workers (create + cleanup), the gRPC event receiver, and the worker-selection scheduler.

## Key types

- `HTTPHandlers` — HTTP layer; `createSandbox` handler opens a transaction, creates the sandbox row and the River job atomically.
- `CreateSandboxWorker` — River worker; drives the sandbox state machine one step per attempt.
- `CleanupSandboxWorker` — River worker; best-effort container removal then marks sandbox failed.
- `EventGRPCServer` — receives Docker events from workers; guards status transitions.
- `Scheduler` — weighted random worker selection; `ChooseLeastUsedWorker` returns `ErrNoWorkerCandidates` when no healthy workers exist.
- `StrongRetryPolicy` — exponential backoff for the default queue (main job).

## Job structure

One River job (`create_sandbox`) drives the sandbox through the state machine. `Work()` reads the current status and executes the NEXT step only:

| Status at start | Side-effect | Status after |
|---|---|---|
| `pending` | none | `scheduling` |
| `scheduling` | pick worker via gRPC GetWorkerInfo | `assigned` (worker_id set) |
| `assigned` | gRPC CreateSandbox | `starting` |
| `starting` | none (event handler may have already advanced) | `running` |
| `running`/terminal | no-op | unchanged |

On the **final attempt**, `maybeCleanup` transitions the sandbox to `cleanup_pending` and enqueues a `cleanup_sandbox` job, then returns `river.JobCancel(...)` so River does not retry.

## State ownership rule

- **Job owns** all transitions from `pending` through `running`.
- **Events own** transitions `starting|running → failed` (die/stop/kill).
- **Cleanup job owns** `cleanup_pending → failed`.

Never let two owners race on the same transition. Guards in `UpdateStatus` and `AssignWorker` enforce this at the SQL level.

## River queue layout

- `default` (MaxWorkers: 10) — create_sandbox jobs; uses `StrongRetryPolicy` (MaxAttempts: 5).
- `cleanup` (MaxWorkers: 5) — cleanup_sandbox jobs; uses River's default retry policy.

## Test patterns

- Use `testutil.SetupRiverClient(t, ctx, pool, addWorkers)` for real River integration.
- Use `testutil.NewFakeWorkerServer(cpuUsage, memUsage)` for a gRPC worker fake; set `.CreateError` to inject failures.
- Direct `Work()` calls (with `fakeCreateJob(...)`) test idempotency without River overhead.
- `waitForStatus` and `waitForTerminal` poll the DB; use a 15s timeout and `FastRetryPolicy` (50ms) to keep tests under a few seconds.

## Gotchas

- `maybeCleanup` uses `log.Printf`. Tests that redirect the global logger (`log.SetOutput`) must restore to `os.Stderr` (not nil) in their defer, otherwise subsequent tests that call `log.Printf` will panic.
- `river.ClientFromContext[pgx.Tx](ctx)` panics if the context doesn't have a River client — only safe inside `Work()`.
- `buildCandidates` calls each worker via gRPC with a 5-second timeout. A slow or down worker is silently skipped (not an error); the scheduler sees fewer candidates.
- The cleanup tx in `maybeCleanup` uses a NEW transaction from `w.pool`, not the River job transaction. This is intentional: the status update and cleanup-job insert must land atomically but independently of the original job's state.
