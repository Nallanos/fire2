# logging package

`SetupLogger(service)` installs the process-wide `slog` default for every binary (`cmd/api`, `cmd/worker`, `cmd/fire2-shim`, `cmd/keepalive`): one JSON object per line on stderr, tagged with the service name, level from `FIRE2_LOG_LEVEL` (debug|info|warn|error, default info; an unknown value warns and falls back to info). It also routes the std `log` package through the handler.

**Standard library only.** `fire2-shim` imports this package, and the shim must not link the orchestrator (River, pgx, chi, gRPC...): their `init()` would run in every VM's shim, the process that must never crash. It used to live in `internal/app`, which is why it moved here. `cmd/fire2-shim/main_test.go` runs `go list -deps` and fails on any package that is not the shim, this one, or `golang.org/x/sys/...`.
