package worker

import (
	"context"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
)

// SandboxManager is what the worker needs from sandboxmgr.Manager. The worker
// never talks to Firecracker itself: every sandbox operation goes through it.
type SandboxManager interface {
	Create(in sandboxmgr.CreateInput) error
	Stop(sandboxID string) error
	Remove(sandboxID string) error
	Snapshot() []*orchestratorv1.SandboxInventoryEntry
	SetCapacity(capacity int)
	Events() <-chan sandboxmgr.StateChange
}

// CreateSandbox boots the sandbox's VM. It is idempotent: a sandbox already
// hosted here is left as is.
func (w *WorkerService) CreateSandbox(ctx context.Context, in CreateSandboxInput) error {
	return w.sandboxes.Create(sandboxmgr.CreateInput{
		ID:         in.ID,
		VcpuCount:  in.VcpuCount,
		MemSizeMib: in.MemSizeMib,
	})
}

// StopSandbox kills the sandbox's VM but keeps its resources until
// RemoveSandbox.
func (w *WorkerService) StopSandbox(sandboxID string) error {
	return w.sandboxes.Stop(sandboxID)
}

// RemoveSandbox stops the sandbox and releases its resources.
func (w *WorkerService) RemoveSandbox(ctx context.Context, sandboxID string) error {
	return w.sandboxes.Remove(sandboxID)
}

// ListRunningSandboxes returns the IDs of every sandbox hosted here.
func (w *WorkerService) ListRunningSandboxes(ctx context.Context) []string {
	snapshot := w.sandboxes.Snapshot()
	ids := make([]string, 0, len(snapshot))
	for _, e := range snapshot {
		ids = append(ids, e.GetSandboxId())
	}
	return ids
}
