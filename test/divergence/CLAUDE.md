# test/divergence

Scenario suite for the question: **when does the database (sandboxes, workers) stop reflecting the machines, and does the system converge on its own?** Results and known holes: `docs/state-divergence.md`.

Everything is behind `//go:build integration` and needs Docker (one Postgres container per scenario). Run it with `go test -tags integration ./test/divergence/`. It is green when no hole is known; a scenario that reveals one stays red (see Conventions), so do not put the suite in a "must be green" gate while one is open.

## Method

Each scenario creates a gap on purpose, lets the real mechanisms run (reapers, inventory reconciliation, fencing, cleanup jobs), then checks the invariant is true again within a bounded delay:

> for every sandbox: the database says `assigned|starting|running` on worker W ⇔ W really hosts the VM (alive).

`assertConverged` checks it and lists every remaining gap; `assertStable` checks that nothing moves during a given time (for scenarios where nothing should happen).

Both sides run their real code. Only Firecracker is replaced, so root and KVM are not needed:

- **Orchestrator**: Postgres, River jobs, gRPC server and HTTP API, wired by `internal/app` (`NewRiverClient`, `NewEventServer`) exactly as `cmd/api` does, with short delays (`cluster.go`: 2s heartbeat timeout and liveness, 5s worker-lost timeout, 30s creation timeout, 200ms reapers, `FastRetryPolicy`).
- **Worker**: `workerpkg.NewWorkerService`, `NewWorkerGRPCServer`, `RunHeartbeat`, `NewEventReporter`, over `FakeVMs` instead of `sandboxmgr.Manager`.

## Files

| File | Role |
|---|---|
| `cluster.go` | `newCluster(t, opts...)` (marks the test parallel, own database), `AddWorker`, `WaitSynced`, HTTP helpers (`Create`, `TryCreate`, `DeleteSandbox`), row readers, `dump()` |
| `fakevms.go` | `FakeVMs`: the `worker.SandboxManager` contract in memory, plus hooks on the machine (`Die`, `Adopt`, `Vanish`, `Wipe`, `SetFull`) and what happened to it (`Hosted`, `Boots`, `RemoveCalls`, `Removed`) |
| `simworker.go` | `SimWorker`: process controls (`Crash`, `Restart`, `KillMachine`), network controls (`Partition`, `LoseCreateResponses`, `SlowCreates`, `StripInventory`), `SetCapacity` |
| `legacy.go` | `LegacyWorker`: a pre-fencing worker (gRPC answers, heartbeats refused) |
| `invariants.go` | `eventually`, `divergences`, `assertConverged`, `assertStable`, `gap` |
| `scenarios_*_test.go` | A (worker lost or cut off), B (restarts and tokens), C+D (creations, deletions, drift), E (capacity) |

## Conventions

- Test names start with the scenario ID (`TestB4_...`): `-run TestB4`.
- A scenario that checks one mechanism disables the competing one (`WithLivenessTimeout(time.Hour)`; `WithCreateTimeout` for D3; `WithWorkerLostTimeout`) so a green result cannot come from the wrong path. By default a sandbox of a silent worker is failed `workerLostTimeout` (5s) after the worker is shown inactive (A2, A3), and an orchestrator outage shorter than that costs no sandbox (A6, which cuts every worker off for 3s).
- A scenario that reveals a hole **stays red**, no `t.Skip`: it fails through `gap(t, "<id>", expected, observed)`, which points at `docs/state-divergence.md#<id>` (B4 did so until it was fixed, and still ends with `gap`). Fixing the hole turns it green; that is the check that the fix works.
- `SimWorker`'s gRPC `GetWorkerInfo` reports zero CPU and memory usage: the host's load must not decide whether the scheduler finds a worker "full".
- `SimWorker.SetCapacity(n)` goes through the real budget computation (`cpu=n`, memory for `n` sandboxes); `AddWorker` asserts the orchestrator ends up with that capacity, so a change to the per-sandbox memory footprint in `worker/monitoring.go` fails loudly (update `sandboxMemMiB` in `simworker.go`).
