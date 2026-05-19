# docker package

Thin wrapper around the Docker SDK. Exposes a narrow `ClientInterface` so the rest of the codebase stays decoupled from Docker implementation details and can be mocked in tests.

## Interface

```go
type ClientInterface interface {
    PullImage(ctx, img) error
    CreateContainer(ctx, img, hostPort, id) (containerID string, error)
    StartContainer(ctx, containerID) error
    StopContainer(ctx, containerID) error
    RemoveContainer(ctx, containerID) error
    InspectImage(ctx, img) error
    Events(ctx) (<-chan EventMessage, <-chan error)
}
```

## Container conventions

- Every container runs `sleep infinity` as its command (containers are kept-alive shells, not services).
- Port `3000/tcp` is exposed internally and bound to `hostPort` on `0.0.0.0`.
- Labels `id` and `sandbox_id` are set to the sandbox UUID — used by `EventReporter` to correlate Docker events.

## Events

`Events(ctx)` filters for `type=container` events only. Returns two channels: one for `EventMessage` values and one for errors. The caller must cancel the context to stop streaming; the `EventMessage` channel is closed on stream end.

## Construction

`NewClient()` reads Docker connection config from the environment (`DOCKER_HOST`, `DOCKER_TLS_VERIFY`, etc.) via `client.FromEnv` and auto-negotiates the API version.
