# sandbox package

Domain model, business logic, and PostgreSQL repository for sandboxes.

## Types

**`Sandbox`** — the core domain struct: `ID`, `Runtime`, `Status`, `TTL`, `CreatedAt`, `Port`, `PreviewURL`, `Image`.

**`Status`** — `queued | running | succeeded | failed`

## Service

`Service` has two modes depending on how it's constructed:

| Constructor | Has Docker? | Use case |
|-------------|-------------|----------|
| `NewService(repo)` | No | Orchestrator — DB reads/writes only |
| `NewRuntimeService(repo, docker)` | Yes | Worker — full container lifecycle |

### Methods

- `Create(ctx, CreateRequest) Sandbox` — inserts a `queued` sandbox; generates UUID internally.
- `GetByID(ctx, id) Sandbox` — returns `ErrNotFound` if missing.
- `List(ctx) []Sandbox` — ordered by ID descending.
- `CreateAndStart(ctx, RuntimeCreateRequest) Sandbox` — pulls image if needed, creates container, starts it, then persists the record. Cleans up the container if the DB write fails.
- `Stop(ctx, containerID)` / `Remove(ctx, containerID)` — stop and optionally remove container + DB record.

## Repository

`PostgresRepository` wraps `db.Querier`. Key behavior: `Create()` is **idempotent** — if a record with the same ID already exists (unique constraint violation, code `23505`), it returns the existing row instead of erroring. This handles the case where the orchestrator pre-creates the record before the worker's gRPC call arrives.

## Errors

| Sentinel | Meaning |
|----------|---------|
| `ErrNotFound` | `GetByID` found no row |
| `ErrDockerClientRequired` | Called `Stop`/`Remove`/`CreateAndStart` without Docker |

Error message constants (`ErrMsgInvalidJSON`, `ErrMsgRuntimeRequired`, etc.) are used by HTTP handlers for consistent response text.
