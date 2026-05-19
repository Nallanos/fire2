# db package

sqlc-generated query layer for PostgreSQL. All files except the manually-added `UpdateSandboxRunning` method are generated — do not hand-edit generated files.

## Code generation

```bash
sqlc generate   # regenerates from internal/db/queries/*.sql
```

**Known issue:** `sqlc generate` currently fails because migrations 003 and 004 wrap DDL in `DO $$ BEGIN...END $$` PL/pgSQL blocks that sqlc can't parse statically. If you add new queries, add the SQL to `internal/db/queries/*.sql` and manually add the corresponding Go code to the generated `.sql.go` file and the `Querier` interface.

## Querier interface

| Method | Table | Notes |
|--------|-------|-------|
| `CreateSandbox` | sandboxes | Full insert |
| `GetSandbox` | sandboxes | By ID |
| `ListSandboxes` | sandboxes | Ordered by id DESC |
| `UpdateSandbox` | sandboxes | Status only |
| `UpdateSandboxRunning` | sandboxes | Status + port + image — **manually added** |
| `DeleteSandbox` | sandboxes | By ID |
| `CreateWorker` | worker | Full insert |
| `GetWorker` | worker | By ID |
| `ListWorkers` | worker | All rows |
| `UpdateWorker` | worker | All mutable fields incl. heartbeat |
| `DeleteWorker` | worker | By ID |
| `CreateSandboxEvent` | sandbox_events | Insert event |

## Migrations

Managed by dbmate in `internal/db/migrations/`. Apply with:
```bash
dbmate --migrations-dir internal/db/migrations up
```

River queue tables are created programmatically at API startup via `rivermigrate` — they are not in the dbmate migrations.

## Connection

The `db.New(sqlDB)` constructor accepts `*sql.DB`. The API server now creates a `*pgxpool.Pool` and wraps it via `stdlib.OpenDBFromPool(pool)` to satisfy this interface while also providing the pool to River.
