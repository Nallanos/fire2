# sandboxmgr package

Owns every sandbox hosted by a worker. The worker (`internal/packages/worker`) goes through it for every sandbox operation and never talks to Firecracker itself.

## Key types

| Type | File | Role |
|------|------|------|
| `Manager` | `manager.go`, `audit.go` | Map of sandboxes, capacity, state snapshot, event stream, audit loop |
| `Service` | `service.go` | Actor of one sandbox: one goroutine owning its VM |
| `vm` | `vm.go` | What a Service drives; `firecrackerVM` (a VM under its `fire2-shim`) in production, a fake in tests |
| `Config` | `vm.go` | `FIRECRACKER_IMAGE_DIR`, `FIRE2_THINPOOL_CONF`, `FIRE2_SHIM_PATH`, `FIRECRACKER_BIN`, sandbox base dir, read once |
| `LockBaseDir` | `lock.go` | Exclusive `flock` on the base dir: one worker process per dir |

## States

`State` is the proto `SandboxState`: `STARTING`, `RUNNING`, `PAUSED`, `UNKNOWN` (process alive, API query failed), `DEAD` (`Wait()` returned: the shim reported Firecracker's exit, or the shim itself died, taking the VM with it). DEAD is final. It is a worker-side state, not a Firecracker one.

A Service reports a state only when it differs from the last one (diff-only). The Manager records it for `Snapshot()` (heartbeat inventory) and pushes a `StateChange` on `Events()` without blocking — a full buffer drops the change; the heartbeat is the guarantee.

## Service (actor)

`run` selects over: commands (create / stop / remove, each with a reply channel), the VM's exit (`Wait`), a 2s state poll, and ctx. Only this goroutine touches the VM, so the VM needs no lock.

- **create** — builds the VM (creates its directory), creates the thin volume (`thin.Pool.CreateVolume`, pure Go), spawns its `fire2-shim`, which boots it from `vm.json`, then waits for RUNNING, bounded (`waitRunning`, 30s, or worker shutdown). On failure it cleans up and the actor exits.
- **stop** — kills the VM; the sandbox stays tracked with its volume and directory, and turns DEAD once the process exits.
- **remove** — stop + volume + directory, then the actor exits.
- A panic is recovered and turns the sandbox DEAD.

## Manager

- **Ownership rule** — an ID in `services` or `deleting` owns its resources on disk; nothing else touches them. `Create` registers the ID (STARTING) *before* its Service touches the disk.
- **Create** is idempotent; an ID in `deleting` gets `ErrBusy`; `len(services)+len(deleting) >= capacity` gets `ErrAtCapacity` (DEAD sandboxes count: they still hold a volume). Capacity 0 means not known yet, no limit.
- **Remove** is the only path that drops a sandbox, DEAD ones included: a DEAD sandbox stays tracked (and reported) until the orchestrator's cleanup removes it. If the actor is already gone, the sandbox is reaped from disk.
- **Recover** (once, at startup): every sandbox directory whose shim is alive is adopted (`Service.adopt`: no create, its exit watched as usual) in its current state; everything else is reaped. A shim whose VM already exited counts as gone (`shim.Attach` refuses it, see `internal/packages/shim/CLAUDE.md`): it is reaped, not adopted as DEAD, and the orchestrator fails it as lost in the first inventory.
- **Run** (audit, every 30s):
  - *liveness* — a tracked sandbox whose Service goroutine exited is marked DEAD;
  - *disk reconciliation* — lists the sandbox directories in the base dir **first**, then, under the lock, claims (`deleting`) every ID not in the map and reaps it (`runtime.ReapSandbox`). Listing before locking is what keeps it from reaping a sandbox being created.

## Constraints

- The sandbox directory is the ownership marker: created before the volume, removed after it, kept if the volume removal fails. Volumes live in the host-wide device-mapper namespace, so nothing reaps by listing volumes.
- One worker per host. Two processes on the same base dir would reap each other's sandboxes: `cmd/worker` takes `LockBaseDir` before reaping anything and exits if another process holds it.
- Only directory names that parse as UUIDs are considered (`runtime.IsSandboxID`): the base dir defaults to `/tmp`.
- A sandbox ID is a path under the base dir that the worker, as root, creates and removes recursively. `Create` and `Remove` refuse any ID that is not a UUID (`runtime.ErrInvalidSandboxID`) before registering or touching anything, as do `runtime.NewClient` and `runtime.Attach`; the gRPC layer turns it into `InvalidArgument`. Do not rely on a later step (the volume removal, whose regex used to be the only guard) to reject a bad ID.
