# fire2

An attempt at building a minimal sandbox-provisioning platform: spin up
per-user, isolated containers (Firecracker microVMs under the hood) on
demand, through a simple API.

It's an **orchestrator** (REST API, auth, scheduling) that hands off
container lifecycle to a fleet of **workers**, each driving its own local
Firecracker host. A small React frontend sits on top for
signup/login and sandbox management.

See [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) for how the pieces fit
together.

## Quick start

```bash
make sandbox-up       # start Postgres
make sandbox-migrate  # apply DB migrations
make sandbox-start    # start API + workers
make sandbox-seed     # create test sandboxes
```

## Development

```bash
make run          # API server (HTTP :8081, gRPC :7001)
make run-worker    # worker server (gRPC :50051)
make test          # run unit tests
```
