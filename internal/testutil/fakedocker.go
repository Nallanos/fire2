package testutil

import (
	"context"
	"sync"

	"github/nallanos/fire2/internal/packages/docker"
)

// FakeDockerClient implements docker.ClientInterface in memory for tests.
// Containers are keyed by sandbox ID (the label passed to CreateContainer).
type FakeDockerClient struct {
	mu         sync.Mutex
	containers map[string]string // sandboxID → containerID

	// Configurable errors; nil means success.
	PullError   error
	CreateError error
	StartError  error
	StopError   error
	RemoveError error

	createCalls map[string]int // sandboxID → CreateContainer call count
}

func NewFakeDockerClient() *FakeDockerClient {
	return &FakeDockerClient{
		containers:  make(map[string]string),
		createCalls: make(map[string]int),
	}
}

func (f *FakeDockerClient) PullImage(_ context.Context, _ string) error { return f.PullError }

// InspectImage always reports the image as present (no pull needed).
func (f *FakeDockerClient) InspectImage(_ context.Context, _ string) error { return nil }

// CreateContainer stores a fake container keyed by sandbox id and returns a container ID.
func (f *FakeDockerClient) CreateContainer(_ context.Context, _ string, _ string, id string) (string, error) {
	if f.CreateError != nil {
		return "", f.CreateError
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	containerID := "fake-" + id
	f.containers[id] = containerID
	f.createCalls[id]++
	return containerID, nil
}

func (f *FakeDockerClient) StartContainer(_ context.Context, _ string) error { return f.StartError }
func (f *FakeDockerClient) StopContainer(_ context.Context, _ string) error  { return f.StopError }

func (f *FakeDockerClient) RemoveContainer(_ context.Context, containerID string) error {
	if f.RemoveError != nil {
		return f.RemoveError
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for sandboxID, cID := range f.containers {
		if cID == containerID {
			delete(f.containers, sandboxID)
			return nil
		}
	}
	return nil // no-op if not found
}

// FindContainerBySandboxID looks up the container by sandbox ID label.
func (f *FakeDockerClient) FindContainerBySandboxID(_ context.Context, sandboxID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[sandboxID], nil
}

// Events returns immediately-closed channels (no events in tests by default).
func (f *FakeDockerClient) Events(_ context.Context) (<-chan docker.EventMessage, <-chan error) {
	ch := make(chan docker.EventMessage)
	errCh := make(chan error)
	close(ch)
	close(errCh)
	return ch, errCh
}

// ContainerExists reports whether a container for the given sandbox ID is present.
func (f *FakeDockerClient) ContainerExists(sandboxID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.containers[sandboxID]
	return ok
}

// CreateCallCount returns how many times CreateContainer was called for sandboxID.
func (f *FakeDockerClient) CreateCallCount(sandboxID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls[sandboxID]
}

// RunningCount returns the number of containers currently tracked.
func (f *FakeDockerClient) RunningCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}
