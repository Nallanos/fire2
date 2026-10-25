# cmd/worker

Entry point for a worker node. Runs the `WorkerService` gRPC server, its sandbox manager and the sandbox state-change reporter.

## What it does

1. Locks the sandbox base dir (`sandboxmgr.LockBaseDir`, exclusive `flock`) and exits if another worker holds it: one worker per host.
2. Takes back the sandboxes a previous crash/restart left running (`Manager.Recover`): each VM runs under its own `fire2-shim`, which survives the worker; live shims are re-attached, everything else is reaped. Done before the first heartbeat, so the orchestrator sees them in this incarnation's first inventory.
3. Connects to the orchestrator over gRPC (`ORCHESTRATOR_GRPC_ADDR`).
4. Starts the sandbox manager's audit loop (goroutine liveness + disk reconciliation every 30s).
5. Creates `WorkerService` over the manager, and starts `EventReporter`, which forwards the manager's state changes to the orchestrator via `OrchestratorService.IngestSandboxEvent`.
6. Serves `WorkerService` gRPC. With `WORKER_PORT` unset or `0` it binds an OS-assigned ephemeral port, reads the actual port back from the listener (`SetListenPort`), and reports it to the orchestrator via heartbeat. A non-zero `WORKER_PORT` pins a specific port.

The worker holds **no database credentials**. It first calls `OrchestratorService.RegisterWorker` to claim a new fencing token, then includes that token in every `ReportWorkerHeartbeat` call. The orchestrator rejects heartbeats from an older worker incarnation (`FailedPrecondition`, final). If its database lost the worker's token (row deleted, restored from a backup) it answers `NotFound` instead and the worker registers again by itself, presenting the token it held.

## Environment variables

| Var | Default |
|-----|---------|
| `WORKER_PORT` | `0` (ephemeral, OS-assigned) |
| `ORCHESTRATOR_GRPC_ADDR` | `127.0.0.1:7001` |
| `WORKER_ID` | hostname |
| `WORKER_ADVERTISED_HOST` | auto-detected |
| `WORKER_HEARTBEAT_INTERVAL` | `5s` |
| `WORKER_CPU_BUDGET` | `0` (auto-detects `runtime.NumCPU()`) |
| `WORKER_MEM_BUDGET` | `0` (auto-detects total RAM from `/proc/meminfo`) |
| `FIRECRACKER_IMAGE_DIR` | — (holds `vmlinux`; checked at each create) |
| `FIRECRACKER_SANDBOX_DIR` | `/tmp` (per-sandbox directories, locked by the worker) |
| `FIRE2_THINPOOL_CONF` | `/etc/fire2/thinpool.conf` (dm-thin pool settings, see `internal/packages/thin`) |
| `FIRE2_SHIM_PATH` | `/opt/fire2/fire2-shim` (one per VM, see `internal/packages/shim`) |
| `FIRECRACKER_BIN` | `/usr/local/bin/firecracker` (run by the shim) |

One worker process runs per host (systemd unit `fire2-worker`), **as root**: it drives device-mapper itself, with no sudo or helper script on the sandbox creation path. It owns every sandbox on the host, so a second process is refused at startup by the base dir lock. Its gRPC API is not authenticated (bound to the private address only) and a sandbox ID becomes a path it creates and removes recursively as root, so every entry point refuses an ID that is not a UUID (`InvalidArgument`).

## VMs outlive the worker

systemd runs the unit with `KillMode=process`: stopping or restarting the worker leaves the shims and their VMs running, and the next worker re-attaches to them. To stop every VM (maintenance), stop the service and run `worker -reap-all` (takes the base dir lock, so it refuses to run next to a live worker), or reboot.

## When the worker gave up on its heartbeats (exit 78)

If the orchestrator refuses the heartbeats for good (`InvalidArgument`, `Unimplemented`, `PermissionDenied`, `Unauthenticated`: typically a worker older than the orchestrator) for 10 minutes straight, `RunHeartbeat` returns `ErrHeartbeatRefused` and the worker exits with status 78. The unit lists 78 in `RestartPreventExitStatus`, so systemd leaves it `failed` instead of restarting it into the same refusal. The VMs keep running under their shims. A *temporary* orchestrator fault that lasts 10 minutes (a bad deploy answering `InvalidArgument`, say) does the same to the whole fleet, and nothing restarts the workers by itself.

Recovery, once the orchestrator accepts the heartbeats again (redeploy it, or redeploy the workers if they are the older side):

```
systemctl reset-failed fire2-worker && systemctl start fire2-worker
```

Run it on every worker that stopped (`systemctl is-failed fire2-worker`). The new process takes the base dir lock, re-attaches to the live shims and registers (a new fencing token).

What to expect: for those 10 minutes the orchestrator sees a silent worker, and after `SANDBOX_WORKER_LOST_TIMEOUT` (2m by default) it fails that worker's sandboxes (their cleanup cannot reach a worker that has exited, so it gives up and drops the worker link). The VMs are still running under their shims; the restarted worker lists them in its first heartbeat, and the orchestrator, which no longer attributes them to it, has them removed as orphans. So the recovery brings the capacity back, not the sandboxes of the outage.
