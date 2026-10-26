package sandbox

import "errors"

var ErrNotFound = errors.New("sandbox not found")

// ErrWorkerFull is returned by AssignWorker when the chosen worker already
// hosts as many sandboxes as its capacity allows. The sandbox stays in
// scheduling so the caller can pick another worker.
var ErrWorkerFull = errors.New("worker has no capacity left")

const (
	ErrMsgInvalidJSON         = "invalid json"
	ErrMsgRuntimeRequired     = "runtime is required"
	ErrMsgIDRequired          = "id is required"
	ErrMsgNotFound            = "not found"
	ErrMsgCreateSandboxFailed = "failed to create sandbox"
	ErrMsgFetchSandboxFailed  = "failed to fetch sandbox"
	ErrMsgListSandboxesFailed = "failed to list sandboxes"
)
