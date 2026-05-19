# github/nallanos/fire2 (modular monolith)

This service orchestrates sandbox containers via workers. The API selects a worker, calls it over gRPC, and persists sandbox metadata in Postgres.

## Run (local)

1) Start Postgres and create the schema (see [internal/db/migrations/001_initial_schema.sql](internal/db/migrations/001_initial_schema.sql)).
2) Export your database URL:

```
export DATABASE_URL=postgresql://temporal:temporal@localhost/temporal
```

3) Start the API:

```
make run
```

4) Start a worker (Docker must be available on the host):

```
make run-worker
```

5) Register the worker in the database (example):

```
go run ./cmd/create_worker --port 50051 --cpu-budget 4 --mem-budget 8192
```

## API

- `GET /health` -> `ok`
- `POST /api/sandboxes` -> create sandbox
  - body:
    ```json
    {
      "runtime": "node",
      "image": "node:20-alpine",
      "port": 10000,
      "ttl": 3600,
      "preview_url": "https://example.invalid"
    }
    ```
  - `image`, `port`, `ttl`, and `preview_url` are optional
- `GET /api/sandboxes` -> list sandboxes
- `GET /api/sandboxes/{id}` -> get sandbox by id
