# Fire

A sandbox orchestration service written in Go. The API accepts sandbox creation
requests, schedules them onto a fleet of workers through a Postgres-backed job queue,
and tracks each sandbox until it is running, stopped or failed. Workers boot each
sandbox as a **Firecracker microVM**.

## Architecture

```
  HTTP client ──► API (cmd/api)                       Worker (cmd/worker) × N
                  · Chi router, auth, ownership        · Firecracker microVMs
                  · River jobs on PostgreSQL ──gRPC──► · capacity limit (CPU / memory budget)
                  · gRPC receiver :7001 ◄──────────── · heartbeat, orphan reaper
                          │
                     PostgreSQL
            (sandboxes, workers, events, users, River tables)
```

Workers and the API talk over gRPC on a private Tailscale network; nothing gRPC is
exposed on a public interface.

### Creating a sandbox

1. `POST /api/sandboxes` inserts the sandbox row (`pending`) **and** enqueues its River
   job **in the same transaction**: a sandbox can never exist without the job that
   drives it, or the reverse.
2. The job moves the sandbox through a state machine:
   `pending → scheduling → assigned → starting → running`. Every transition is a
   conditional update on the expected previous status, so a retried or duplicated job
   cannot move a sandbox backwards.
3. The scheduler skips workers with a stale heartbeat, asks the others for their live
   load over gRPC, and picks one by weighted random draw favouring the least used. The
   chosen worker boots the microVM (kernel + root filesystem, per-sandbox vCPU and
   memory size) and refuses the call if it is already at capacity.
4. Failures are retried with exponential backoff (5 attempts, about 60 s worst case).
   When attempts are exhausted, a cleanup job tears the VM down and marks the sandbox
   `failed`. `DELETE` reuses the same cleanup path and ends in `stopped`.

The HTTP call subscribes to River completion events before inserting, so it returns
`201` once the sandbox is running, or an error, without polling.

### Keeping the fleet honest

- Workers send heartbeats to the API over gRPC. A periodic **reaper** job demotes workers
  whose heartbeat went stale, so they stop receiving sandboxes; a worker that comes back
  re-registers on its next heartbeat.
- On startup, a worker **reaps orphaned VMs** left behind by a crash (leftover sandbox
  directories and Firecracker processes).
- Workers never touch the database: everything goes through the API.

## Sandbox status

| Status | Meaning |
|--------|---------|
| `pending` | Row and job created |
| `scheduling` | Selecting a worker |
| `assigned` | Worker chosen, gRPC call in flight |
| `starting` | microVM created, booting |
| `running` | microVM is up |
| `cleanup_pending` | Cleanup job running (retries exhausted, or deleted) |
| `failed` | Terminal failure |
| `stopped` | Deleted by its owner |

## API

All `/api/sandboxes` routes require a bearer token (`Authorization: Bearer <token>`)
and only ever return the caller's own sandboxes.

| Method | Route | |
|--------|-------|-|
| `GET` | `/health` | `ok` |
| `POST` | `/api/auth/signup` · `/api/auth/login` · `/api/auth/logout` | session token |
| `POST` | `/api/sandboxes` | create, blocks until running (~45 s timeout) |
| `GET` | `/api/sandboxes` | list, newest first |
| `GET` | `/api/sandboxes/{id}` | one sandbox, `404` if not yours |
| `DELETE` | `/api/sandboxes/{id}` | stop and clean up |

```json
POST /api/sandboxes
{ "runtime": "node", "ttl": 3600, "vcpu_count": 1, "mem_size_mib": 256 }
```

`runtime` is required; `ttl` defaults to 3600 s, `vcpu_count` to 1, `mem_size_mib` to 256.

## Project status

The orchestration layer (API, job queue, state machine, retries, cleanup, heartbeats,
auth) is complete and covered by integration tests. The Firecracker runtime boots VMs
end to end on provisioned workers, but the root filesystem is still a minimal Alpine
smoke-test image: per-runtime images (Node, Python, Go) are the next step, and the
`runtime` / `image` fields are not yet mapped to them.

## Running it

Requirements: Go 1.23+, PostgreSQL, [dbmate](https://github.com/amacneil/dbmate), and
for workers a Linux host with KVM and Firecracker.

```bash
make sandbox-up        # PostgreSQL via docker compose
make sandbox-migrate   # apply migrations (River creates its tables at API startup)
make sandbox-start     # API + workers in the background, logs in .sandbox/
make sandbox-smoke     # smoke-test the endpoints
```

| Variable | Used by | |
|----------|---------|-|
| `DATABASE_URL` | API | Postgres connection string |
| `PORT` | API | HTTP port |
| `ORCHESTRATOR_GRPC_PORT` · `ORCHESTRATOR_GRPC_BIND_HOST` | API | gRPC receiver |
| `HEARTBEAT_TIMEOUT` · `REAPER_INTERVAL` | API | worker liveness |
| `ORCHESTRATOR_GRPC_ADDR` | worker | where to reach the API |
| `WORKER_ID` · `WORKER_PORT` · `WORKER_GRPC_BIND_HOST` · `WORKER_ADVERTISED_HOST` | worker | identity and gRPC listener |
| `WORKER_CPU_BUDGET` · `WORKER_MEM_BUDGET` | worker | capacity |
| `FIRECRACKER_IMAGE_DIR` | worker | directory holding `vmlinux` and `rootfs.ext4` |
| `FIRECRACKER_SANDBOX_DIR` | worker | per-sandbox working directories (default `/tmp`) |

### Deploying workers

`ansible/playbook.yml` provisions a worker VM end to end: Firecracker, kernel and root
filesystem images, Tailscale, a dedicated user and systemd units (several workers per
host supported). `make ansible-deploy` runs it, `make ansible-smoke` checks the result.

## Development

```bash
make test                          # unit tests
go test -tags integration ./...    # integration tests against a real Postgres (Docker)
make proto                         # regenerate gRPC code
```

SQL lives in `internal/db` (migrations with dbmate, queries generated with sqlc).
`web/` holds a small React front end for signup and sandbox management.
