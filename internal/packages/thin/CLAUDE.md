# thin package

Creates and removes sandbox root drives in the fire2 dm-thin pool, in pure Go (device-mapper ioctls through `golang.org/x/sys/unix`, `CGO_ENABLED=0`). No dmsetup, no shell, no sudo on the sandbox creation path: the worker runs as root.

## Files

| File | Role |
|------|------|
| `dm.go` | Device-mapper ioctl layer on `/dev/mapper/control`: `struct dm_ioctl` header, target specs, `createDevice` (create + load + resume), `removeDevice` (retries `EBUSY` like `dmsetup remove --retry`), `table`, `message` |
| `pool.go` | `Pool` (from `thinpool.conf`), `CreateVolume` / `RemoveVolume` |

## Volumes

- `CreateVolume(id)`: reads `base.env` (base thin ID + size, swapped by `fire2-thinpool-import-base`), sends `create_snap <random id> <base id>` to the pool (retries on `EEXIST`), creates `fire2-sb-<id>` with table `0 <sectors> thin /dev/mapper/<pool> <id>`, and `mknod`s `/dev/mapper/fire2-sb-<id>` itself rather than wait for udev. Returns that path for Firecracker.
- `RemoveVolume(id)`: idempotent. Reads the device's table and refuses anything but a single `thin` target with an ID ≥ `FIRST_SANDBOX_ID` (never deletes a base volume), removes the device and its node, then sends `delete <thin id>` to the pool. A failed `delete` leaks the thin ID until the next boot, when `fire2-thinpool-up` drops every sandbox ID.

## Cold path (still bash, Ansible)

`fire2-thinpool-up` (pool activation at boot) and `fire2-thinpool-import-base` (base volume import) stay shell scripts; they share `/etc/fire2/thinpool.conf` with this package.

## Tests

- Encoding and parsing tests run anywhere.
- `TestControlRoundTrip` needs root (a `zero` target, no pool).
- `TestPoolCreateRemove` needs root and a pool described by `$FIRE2_TEST_THINPOOL_CONF`. Runs in a privileged container with `-v /lib/modules:/lib/modules:ro` (loop files + `dmsetup create ... thin-pool`, `create_thin 1` as base).
