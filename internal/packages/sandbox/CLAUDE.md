# sandbox package

## Purpose

Domain model and persistence for sandboxes. Owns the status state machine and enforces that status advances only via guarded SQL updates (WHERE status IN (...)). The worker side does NOT write the sandbox row — that responsibility belongs entirely to the orchestrator.

## Key types

- `Sandbox` — domain struct; `WorkerID *string` is nullable (nil until scheduling step assigns a worker).
- `Status` — string typedef; use the `Status*` constants, never raw strings.
- `Repository` — interface; both the postgres impl and any test fake must satisfy it.
- `PostgresRepository` — pgx/v5 implementation; accepts `pgxdb.DBTX` so it works inside a `pgx.Tx`.

## Status state machine

```
pending → scheduling → assigned → starting → running → (user stopped/failed)
                                                 ↓
                                          cleanup_pending → cleaned_up
                                               ↓
                                            failed
```

- **Job owns**: `pending → scheduling → assigned → starting → running`
- **Event handler owns**: `starting|running → failed` (die/stop/kill events)
- **Cleanup job owns**: `cleanup_pending → failed`

## Invariants

- Status only advances via `UpdateStatus` or `AssignWorker` with an explicit `allowedFrom` set. `rowsAffected=0` means the guard rejected the update — callers must handle this (not an error).
- `AssignWorker` sets `worker_id` and transitions `scheduling → assigned` atomically in one SQL statement.
- `ClearWorker` sets `worker_id = NULL` unconditionally. Used by cleanup worker before marking failed.
- **Worker side must NOT write the sandbox row.** The container exists on the worker; the row lives in the orchestrator DB. The only time the worker touches the sandbox table is never.

## Test patterns

- Use `testutil.SetupPostgres(t, ctx)` for a real Postgres testcontainer.
- Seed rows with `repo.Create(ctx, sandbox.Sandbox{...})` directly.
- To test guards: call `UpdateStatus` with an unexpected current status and assert `rowsAffected == 0`.
- `WithTx(tx)` lets you test transactional behavior: verify visibility inside the tx, then commit and verify persistence.

## Gotchas

- `UpdateStatus` returns `(Sandbox, 0, nil)` when the guard rejects (no match). This is NOT an error — callers must check the second return value.
- `scanSandbox` reads exactly 9 columns in a fixed order. If you add columns to the schema, update the SELECT lists and `scanSandbox` together.
- `itoa` only handles up to 2-digit numbers (placeholders $3–$12). That's enough for the current max of 9 `allowedFrom` states.
