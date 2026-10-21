# shim package (+ cmd/fire2-shim)

Keeps a Firecracker VM alive independently of the worker. One `fire2-shim` process per VM, spawned detached by the worker (`Setsid`), direct parent of Firecracker. A worker crash, restart or redeploy leaves VMs running; the next worker re-attaches (`Attach`) instead of reaping them.

**Not** what restarts the worker: systemd does that. The shim does the opposite job, keeping VMs alive while the worker is gone.

## Files in the sandbox dir

| File | Written by | Role |
|---|---|---|
| `vm.json` | worker | Firecracker `--config-file`: boot-source, root drive, machine-config. The VM boots with no API calls |
| `shim.pid` | shim (atomic rename) | shim PID, read by `Attach` / `KillStale` |
| `shim.sock` | shim | control socket, listened on before Firecracker starts |
| `shim.exit` | shim (atomic rename) | Firecracker's exit code (`-1` if it never started), written before clients are told. Lets a worker that connects after the exit still learn it; removed by `Spawn` |
| `firecracker.sock` | Firecracker | API, used by the worker for `GET /` (state) |
| `firecracker.log` | shim | Firecracker stdout/stderr, serial console included (truncated at each start) |
| `shim.log` | worker | the shim's own stdout/stderr |

## Protocol

A client connects to `shim.sock`; the shim writes exactly one line, `exited <code>\n`, when Firecracker exits (code 128+N if killed by signal N), then closes. **EOF without that line = the shim died**, and the VM with it (`Pdeathsig`). The same code is also recorded in `shim.exit` first: if Firecracker exits before `Spawn` ever connects (a bad config does so in ~3ms), `Spawn` returns an already-done `Handle` carrying that code instead of an error. No `shim.exit` and no socket = the shim itself died. To stop the VM: `SIGTERM` to the shim, which forwards it to Firecracker and sends `SIGKILL` after 3s. Keep this backward compatible: old shims keep running across worker upgrades.

## Invariants

- Firecracker runs with `Pdeathsig: SIGKILL`, started from a goroutine that `LockOSThread`s for its whole life (Pdeathsig follows the forking *thread*). A VM never outlives its shim, so the worker can never lose track of one.
- The shim is never given Pdeathsig: it must outlive the worker.
- `Attach` and `KillStale` check `/proc/<pid>/cmdline` (`fire2-shim ... -dir <dir>`): a recycled PID is never mistaken for a shim.
- Standard library + `x/sys` only. This is the process that must never crash.

## Tests

The test binary doubles as the shim and as a fake Firecracker (`FIRE2_SHIM_TEST_HELPER=1`, role picked from argv), so the real process tree, signals and Pdeathsig are exercised without root or KVM. Same trick in `internal/packages/runtime` for the worker-side end-to-end test (`TestVMSurvivesTheWorker`).
