# docker package

## Purpose

Thin abstraction over the Docker SDK. `ClientInterface` lets tests inject a fake; the real `Client` wraps `client.Client` from `github.com/docker/docker/client`.

## Key types

- `ClientInterface` — interface that all code should depend on (never the concrete `*Client`).
- `Client` — real Docker SDK wrapper; use `NewClient()` to create.

## Key methods

- `CreateContainer(ctx, id, image, ...)` — creates and returns a container ID. Does NOT start it.
- `StartContainer(ctx, containerID)` — starts an already-created container.
- `FindContainerBySandboxID(ctx, sandboxID)` — returns the container ID if a container named after the sandbox exists, empty string otherwise. Used for idempotency in WorkerService.
- `Events(ctx)` — returns a channel of Docker events and a channel of errors. Used by the event relay.

## Container naming convention

Containers are named after their sandbox ID (e.g. `sbx-abc123`). `FindContainerBySandboxID` uses this convention to check existence before creating.

## Test patterns

Use `testutil.NewFakeDockerClient()` in tests. It implements `ClientInterface` with an in-memory map. Key behaviors:

- `CreateContainer` stores `"fake-" + sandboxID` keyed by sandbox ID.
- `FindContainerBySandboxID` looks up by sandbox ID.
- `InspectImage` always returns nil (image "present") to skip pull.
- `Events` returns closed channels (no events in tests).
- `ContainerExists(sandboxID)`, `CreateCallCount(sandboxID)`, `RunningCount()` for test assertions.

## Gotchas

- `CreateContainer` errors (like "name conflict") are not automatically retried. Callers must check for existing containers via `FindContainerBySandboxID` before creating.
- `Events` channel is unbuffered; consumers must read promptly or use a goroutine. The real Docker client will block if the channel fills.
