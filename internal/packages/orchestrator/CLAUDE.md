# orchestrator package

## Purpose

Coordinates sandbox lifecycle across the API, River job queue, Firecracker workers, and the event stream. Owns the create-sandbox HTTP handler, both River job workers (create + cleanup), the gRPC event receiver, and the worker-selection scheduler.

## Key types

- `HTTPHandlers` — HTTP layer; `createSandbox` handler opens a transaction, creates the sandbox row and the River job atomically.
- `CreateSandboxWorker` — River worker; drives the sandbox state machine one step per attempt.
- `CleanupSandboxWorker` — River worker; removes the sandbox from its worker, then moves it to the job's `TargetStatus` (default `failed`, `stopped` for TTL/user delete). Removal must be confirmed by the worker (gRPC `NotFound` counts as confirmed): otherwise the sandbox stays `cleanup_pending` with its `worker_id` and the job errors so River retries. On the final attempt the worker link is dropped and the sandbox ends `failed`.
- `ReapSandboxWorker` — periodic (`reap_sandbox` queue, `SANDBOX_REAPER_INTERVAL`, default 1m). In one transaction it claims (`→ cleanup_pending`, `FOR UPDATE SKIP LOCKED`) sandboxes whose `created_at + ttl` (seconds, `<= 0` = immediately) passed (cleanup → `stopped`) and active sandboxes not listed by their worker for `SANDBOX_LIVENESS_TIMEOUT` (default 15s; cleanup → `failed`), and enqueues their cleanup jobs.
- `RemoveOrphanWorker` — removes a sandbox a worker reports but the DB does not attribute to it (unknown, terminal, or assigned elsewhere); unique per (worker, sandbox) while pending.
- `EventGRPCServer` — receives sandbox state-change events and heartbeats from workers; guards status transitions.
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
- **Events and the heartbeat inventory** both advance `starting → running`
  (guarded, so whichever lands first wins). Events are best-effort: the
  worker sends one per state change and never retries.
- **Heartbeat inventory owns** `* → cleanup_pending` for a sandbox the worker
  reports `DEAD` (its Firecracker process exited). Events carry DEAD too but
  only store it — `statusTransitionForState` has no failure case.
- **Reaper owns** `* → cleanup_pending` for TTL/liveness expiry; **cleanup job owns** `cleanup_pending → failed|stopped`.

Never let two owners race on the same transition. Guards in `UpdateStatus` and `AssignWorker` enforce this at the SQL level.

## Heartbeat reconciliation and fencing

- `ReportWorkerHeartbeat` with `inventory_complete=true` runs in ONE transaction: fenced `worker` update (stale token → `FailedPrecondition`, everything rolled back), `ClaimInventory` (true once per token), `ReconcileInventory`, orphan job inserts. Needs `WithTransactions(pool)`; orphan removal needs `WithRiverClient`.
- Reconcile: the inventory is the worker's sandbox manager snapshot, one `SandboxInventoryEntry{sandbox_id, state}` per tracked sandbox (no VM probe on the heartbeat path). `inventoryFromEntries` splits it into a `sandbox.Inventory` by state:

  | State | `last_seen_at` | Status |
  |---|---|---|
  | `STARTING`, `PAUSED` | refreshed | unchanged |
  | `RUNNING` | refreshed | `starting → running` |
  | `UNKNOWN` (or unspecified) | not refreshed: ages toward liveness expiry | unchanged |
  | `DEAD` | not refreshed | `assigned\|starting\|running → cleanup_pending`, cleanup (→ `failed`) enqueued in the same transaction, like Lost |

  Sandboxes absent from the inventory are not touched — they age until the reaper fails them. A DEAD sandbox stays in the worker's inventory until the cleanup's `RemoveSandbox` drops it, so a crash is acted on within one heartbeat instead of the liveness delay.
- First complete inventory of a new token (`worker.inventory_token`): a new token proves the previous process, and its VMs (ReapOrphans), are dead. Active sandboxes not listed and last seen before `worker.registered_at` are claimed (`cleanup_pending`) and their cleanup (→ `failed`) enqueued in the same transaction, without waiting for the liveness delay. Sandboxes assigned after the registration are spared.
- `inventory_complete=false` (absent inventory) only updates the worker row. Inventory heartbeats need both `WithTransactions(pool)` and `WithRiverClient`.
- `IngestSandboxEvent` requires `fencing_token`. The `sandbox_events` INSERT itself is fenced (`INSERT … SELECT … FROM worker WHERE id AND fencing_token`; no row → `ErrEventFenced` → `FailedPrecondition`), and status changes use `UpdateStatusFenced` (sandbox must be hosted by that worker incarnation).

## River queue layout

- `default` (MaxWorkers: 10) — create_sandbox jobs; uses `StrongRetryPolicy` (MaxAttempts: 5).
- `cleanup` (MaxWorkers: 5) — cleanup_sandbox and remove_orphan_sandbox jobs.
- `reap_sandbox` (MaxWorkers: 1) — the periodic sandbox reaper.

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
