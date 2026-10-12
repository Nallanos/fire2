# db package

**Dead.** `internal/db/db/` (the sqlc-generated `db.Queries` layer this doc used to describe) is empty — untracked in git, never regenerated, and nothing in the tree imports it anymore (confirmed by grepping for `internal/fire2/internal/db` imports and by `go build -tags integration ./...` passing clean). Every repository (`sandbox`, `worker`, `auth`, `orchestrator/event_repository`) now talks to Postgres directly via hand-written `pgx` queries instead — see e.g. `internal/packages/sandbox/repository_postgres.go` for the pattern.

If you're tempted to run `sqlc generate` to bring this back: don't, unless you're deliberately reintroducing the sqlc layer. It was known-broken anyway (migrations 003/004 wrap DDL in `DO $$ ... END $$` blocks sqlc can't parse statically) and the last two callers referencing it (`internal/app/sandbox_flow_integration_test.go`, `internal/app/river_retry_integration_test.go`) were deleted rather than ported, since the pgx-based repositories already cover the same ground.

## Migrations

Managed by dbmate in `internal/db/migrations/`. Apply with:
```bash
dbmate --migrations-dir internal/db/migrations up
```

River queue tables are created programmatically at API startup via `rivermigrate` — they are not in the dbmate migrations.

## Connection

The `db.New(sqlDB)` constructor accepts `*sql.DB`. The API server now creates a `*pgxpool.Pool` and wraps it via `stdlib.OpenDBFromPool(pool)` to satisfy this interface while also providing the pool to River.
