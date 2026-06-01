# cmd/create_worker

One-shot CLI to register a worker in the database. Run once per worker node before starting the worker binary.

## Usage

```bash
create_worker \
  --host 127.0.0.1 \
  --port 50051 \
  --cpu-budget 4 \
  --mem-budget 4096 \
  [--id <uuid>]          # auto-generated if omitted
  [--status active]
  [--db-url <url>]       # falls back to DATABASE_URL env var
```

## Behavior

- Attempts `CreateWorker` INSERT.
- If the ID already exists (prior registration), falls back to `UpdateWorker` — safe to re-run to update budgets or status.
- Exits non-zero if both insert and update fail.
- Prints a summary line on success: `worker ready: id=... host=... port=... cpu_budget=... mem_budget=...`

## Makefile shortcut

```bash
make sandbox-register-worker
```
