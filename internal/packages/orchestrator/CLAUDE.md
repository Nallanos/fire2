# orchestrator package

## Purpose

Coordinates sandbox lifecycle across the API, River job queue, Firecracker workers, and the event stream. Owns the create-sandbox HTTP handler, both River job workers (create + cleanup), the gRPC event receiver, and the worker-selection scheduler.

## Key types

- `HTTPHandlers` — HTTP layer; `createSandbox` handler opens a transaction, creates the sandbox row and the River job atomically. It first refuses with `400` a `vcpu_count` or `mem_size_mib` above one worker slot (`worker.CheckSandboxSize`: 1 vCPU / 256 MiB, which are also the defaults): a worker's capacity counts sandboxes, so a larger one would pass for a single slot on a worker that cannot hold it.
- `CreateSandboxWorker` — River worker; drives the sandbox state machine one step per attempt.
- `CleanupSandboxWorker` — River worker; removes the sandbox from its worker, then moves it to the job's `TargetStatus` (default `failed`, `stopped` for TTL/user delete). Removal must be confirmed by the worker (gRPC `NotFound` counts as confirmed): otherwise the sandbox stays `cleanup_pending` with its `worker_id` and the job errors so River retries. On the final attempt the worker link is dropped and the sandbox ends `failed`.
- `ReapSandboxWorker` — periodic (`reap_sandbox` queue, `SANDBOX_REAPER_INTERVAL`, default 1m). In one transaction it claims (`→ cleanup_pending`, `FOR UPDATE SKIP LOCKED`) sandboxes whose `created_at + ttl` (seconds, `<= 0` = immediately) passed (cleanup → `stopped`) active sandboxes their worker no longer vouches for (liveness, below; cleanup → `failed`), and sandboxes still `pending`/`scheduling` after `SANDBOX_CREATE_TIMEOUT` (default 5m, `0` turns the check off; cleanup → `failed`: their create job gave up without a trace and liveness cannot see them, they have no worker), and enqueues their cleanup jobs.
- **Liveness** (`sandbox.ClaimLivenessExpired`) is measured against the sandbox's *worker*, never the orchestrator's clock: an API restart, a Postgres failover or a network cut between the orchestrator and every worker records no heartbeat and no inventory, and must not cost a sandbox. A sandbox is claimed when (a) its worker keeps heartbeating but has not listed it for `SANDBOX_LIVENESS_TIMEOUT` (default 15s): `worker.last_heartbeat > GREATEST(COALESCE(last_seen_at, created_at), worker.registered_at) + timeout`, all written by the database's `now()` (`last_heartbeat` and `last_seen_at` in one transaction when the sandbox is listed). The count starts at the worker's registration when that is later: a process that has just registered has not yet had its first heartbeat, and the sandboxes of the process before it are for the first inventory to find (Lost), not for liveness; or (b) its worker has been `inactive` for longer than `SANDBOX_WORKER_LOST_TIMEOUT` (default 2m): `worker.inactive_since < now() - timeout`. Rule (a) covers a VM that vanished (D1, E1), `UNKNOWN` states and heartbeats without an inventory (B5); rule (b) a machine that is gone (A2) or a worker the orchestrator keeps refusing (A3). After any outage the worker reaper marks every worker inactive on its first run (`RunOnStart`), then the workers heartbeat back to active within one interval and clear `inactive_since`: only a worker that stays silent for the whole timeout loses its sandboxes. `SANDBOX_WORKER_LOST_TIMEOUT` must exceed the longest outage you accept to ride out.
- `RemoveOrphanWorker` — removes a sandbox a worker reports but the DB does not attribute to it (unknown, terminal, or assigned elsewhere); unique per (worker, sandbox) while pending.
- `EventGRPCServer` — receives sandbox state-change events and heartbeats from workers; guards status transitions.
- `Scheduler` — weighted random worker selection; `ChooseLeastUsedWorker` returns `ErrNoWorkerCandidates` when no healthy workers exist, `ErrClusterFull` when every active one is saturated. A worker's `cpu_usage` is the machine's load in **percent** (0 to 100), so the CPU ratio is `cpu_usage / 100` (`cpuUsageScale`), never divided by `cpu_budget`, which counts cores; `mem_usage` and `mem_budget` are both MiB, so memory is `mem_usage / mem_budget`. Dividing the percentage by the cores judged a 2-core worker full at 2% load.
- `StrongRetryPolicy` — exponential backoff for the default queue (main job).

## Job structure

One River job (`create_sandbox`) drives the sandbox through the state machine. `Work()` reads the current status and executes the NEXT step only:

| Status at start | Side-effect | Status after |
|---|---|---|
| `pending` | none | `scheduling` |
| `scheduling` | pick worker via gRPC GetWorkerInfo | `assigned` (worker_id set) |
| `assigned` | gRPC CreateSandbox | `starting` |
| `starting` | none (event handler may have already advanced) | `running` |
| `running` | none (already up) | unchanged |
| `failed`, `stopped`, `cleanup_pending`, `cleaned_up` | none: the sandbox ended under the job (liveness, TTL, delete, dead VM) | unchanged; the job is **cancelled** (`river.JobCancel`), never completed, so the HTTP handler answers 502 instead of 201 |

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

- `ReportWorkerHeartbeat` with `inventory_complete=true` runs in ONE transaction: fenced `worker` update (refused → everything rolled back, see below), `ClaimInventory` (true once per token), `ReconcileInventory`, orphan job inserts. Needs `WithTransactions(pool)`; orphan removal needs `WithRiverClient`.
- A heartbeat whose token matches no row is refused in one of two ways, told apart by `worker.Repository.Update` re-reading the row's token in the same transaction. **Behind** the row's (`ErrFenced` → `FailedPrecondition`): an older process, refused for good, it must never register again (B3). Row **absent**, or its token **behind** the presented one (`ErrWorkerUnknown` → `NotFound`, "worker unknown, register again"): the database is late (row deleted, restored from an older backup), the worker registers again (B4). `RegisterWorker` reads the worker's last token from the request's `fencing_token` and sets `GREATEST(persisted, presented) + 1`, so the new token is above any token a process ever held and an older process stays behind it.
- Reconcile: the inventory is the worker's sandbox manager snapshot, one `SandboxInventoryEntry{sandbox_id, state}` per tracked sandbox (no VM probe on the heartbeat path). `inventoryFromEntries` splits it into a `sandbox.Inventory` by state:

  | State | `last_seen_at` | Status |
  |---|---|---|
  | `STARTING`, `PAUSED` | refreshed | unchanged |
  | `RUNNING` | refreshed | `starting → running` |
  | `UNKNOWN` (or unspecified) | not refreshed: ages toward liveness expiry | unchanged |
  | `DEAD` | not refreshed | `assigned\|starting\|running → cleanup_pending`, cleanup (→ `failed`) enqueued in the same transaction, like Lost |

  Sandboxes absent from the inventory are not touched — they age until the reaper fails them. A DEAD sandbox stays in the worker's inventory until the cleanup's `RemoveSandbox` drops it, so a crash is acted on within one heartbeat instead of the liveness delay.
- First complete inventory of a new token (`worker.inventory_token`): a new token proves the previous process is dead. Its VMs survive under their shims (`fire2-shim`) and the new process re-attaches to them before its first heartbeat, so this inventory lists every VM still alive. Active sandboxes not listed and last seen before `worker.registered_at` are claimed (`cleanup_pending`) and their cleanup (→ `failed`) enqueued in the same transaction, without waiting for the liveness delay. Sandboxes assigned after the registration are spared.
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
- `buildCandidates` calls each worker via gRPC with a 5-second timeout (`candidatesTimeout`, for all candidates together). A slow or down worker is silently skipped (not an error); the scheduler sees fewer candidates.
- Every other call to a worker has its own deadline, so a worker the network drops packets to (not one that refuses the connection) cannot hold a job until River's timeout: `CreateSandbox` 45s per attempt (`createSandboxCallTimeout`; the worker bounds its boot to 30s), and `removeSandboxOnWorker` 15s from the worker read to the answer (`removeSandboxCallTimeout`: stop with 3s of grace then kill, device-mapper `EBUSY` retries up to 5s), which also covers the orphan removal. A timeout is an error: the create job retries from `assigned` (the worker's `Create` is idempotent), the cleanup stays `cleanup_pending` until a removal is confirmed. `orchestrator.JobTimeout` (80s = 5s + 45s + 30s of room) is set explicitly on the River client in `app.NewRiverClient`; River's default (1m) would leave 10s for the database round trips of a full attempt.
- `SANDBOX_CREATE_TIMEOUT` (5m) only has to outlast `pending`/`scheduling`: the scheduling step (about a minute with its retries) plus queue wait. The 45s `CreateSandbox` call is after `assigned`, where liveness takes over.
- The cleanup tx in `maybeCleanup` uses a NEW transaction from `w.pool`, not the River job transaction. This is intentional: the status update and cleanup-job insert must land atomically but independently of the original job's state.
