# pgxdb package

## Purpose

Defines the `DBTX` interface that both `*pgxpool.Pool` and `pgx.Tx` satisfy. This single interface enables repositories to participate in transactions without knowing whether they're running inside a transaction or directly against the pool.

## Key types

- `DBTX` — interface with `Exec`, `Query`, `QueryRow`. Both `*pgxpool.Pool` and `pgx.Tx` satisfy it.

## Pattern: WithTx

Each repository exposes a `WithTx(tx pgx.Tx) Repository` method that returns a new repo instance wrapping the transaction:

```go
func (r *PostgresRepository) WithTx(tx pgx.Tx) Repository {
    return &PostgresRepository{db: tx}
}
```

This allows atomic multi-repo operations:

```go
tx, _ := pool.Begin(ctx)
defer tx.Rollback(ctx)
sandboxRepo.WithTx(tx).Create(ctx, sbx)
riverClient.InsertTx(ctx, tx, args, nil)
tx.Commit(ctx)
```

## Why we removed sqlc

sqlc generated code assumed `*sql.DB` / `*sql.Tx` and required a separate querier interface. With pgx/v5 and River (which only supports pgx natively), maintaining sqlc was pure overhead. Hand-written pgx queries are more readable and directly testable against real Postgres.

## Gotchas

- `pgx.Tx` is an interface; `*pgxpool.Tx` is the concrete type returned by `pool.Begin`. Both satisfy `DBTX`.
- Do NOT add application-level logic to `DBTX`. It is purely a structural interface for duck-typing pool and tx.
