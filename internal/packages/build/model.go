package build

import (
	"time"
)

type Status string

const (
	StatusQueued   Status = "queued"
	StatusRunning  Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed   Status = "failed"
)

type Build struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Ref       string    `json:"ref"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
