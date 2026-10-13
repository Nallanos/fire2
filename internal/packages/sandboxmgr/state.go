// Package sandboxmgr owns every sandbox hosted by this worker.
//
// A Manager keeps the map of sandboxes and runs one Service per sandbox. A
// Service is an actor: a single goroutine that owns the sandbox's VM, applies
// every write to it (create, stop, remove) and watches its state. State
// changes flow Service → Manager → worker → orchestrator.
package sandboxmgr

import (
	"errors"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
)

// State is the closed sandbox state vocabulary shared with the orchestrator.
type State = orchestratorv1.SandboxState

const (
	StateStarting = orchestratorv1.SandboxState_SANDBOX_STATE_STARTING
	StateRunning  = orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING
	StatePaused   = orchestratorv1.SandboxState_SANDBOX_STATE_PAUSED
	StateUnknown  = orchestratorv1.SandboxState_SANDBOX_STATE_UNKNOWN
	StateDead     = orchestratorv1.SandboxState_SANDBOX_STATE_DEAD
)

// StateChange is emitted by the Manager each time a sandbox's state differs
// from the last one it reported.
type StateChange struct {
	SandboxID string
	State     State
	At        time.Time
}

var (
	// ErrSandboxNotFound is returned when the manager does not track the
	// sandbox (never created here, or already removed).
	ErrSandboxNotFound = errors.New("sandbox not found on worker")
	// ErrBusy is returned when the sandbox ID is being removed or reaped.
	// The caller should retry once the removal is over.
	ErrBusy = errors.New("sandbox is being removed")
	// ErrAtCapacity is returned when the worker hosts as many sandboxes as
	// its capacity allows.
	ErrAtCapacity = errors.New("worker at full capacity")

	// errServiceExited is returned when a command reaches a Service whose
	// goroutine has already exited.
	errServiceExited = errors.New("sandbox service exited")
)

// CreateInput carries what a Service needs to boot a sandbox's VM.
type CreateInput struct {
	ID         string
	VcpuCount  int32
	MemSizeMib int32
}
