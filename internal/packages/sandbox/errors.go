package sandbox

import "errors"

var ErrNotFound = errors.New("sandbox not found")

const (
	ErrMsgInvalidJSON         = "invalid json"
	ErrMsgRuntimeRequired     = "runtime is required"
	ErrMsgIDRequired          = "id is required"
	ErrMsgNotFound            = "not found"
	ErrMsgCreateSandboxFailed = "failed to create sandbox"
	ErrMsgFetchSandboxFailed  = "failed to fetch sandbox"
	ErrMsgListSandboxesFailed = "failed to list sandboxes"
)
