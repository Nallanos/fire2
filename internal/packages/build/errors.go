package build

import "errors"

var ErrNotFound = errors.New("build not found")

const (
	ErrMsgInvalidJSON       = "invalid json"
	ErrMsgRepoRequired      = "repo is required"
	ErrMsgIDRequired        = "id is required"
	ErrMsgNotFound          = "not found"
	ErrMsgCreateBuildFailed = "failed to create build"
	ErrMsgFetchBuildFailed  = "failed to fetch build"
	ErrMsgListBuildsFailed  = "failed to list builds"
)
