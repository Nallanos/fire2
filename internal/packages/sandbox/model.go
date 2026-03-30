package sandbox

import "time"

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Sandbox struct {
	ID         string    `json:"id"`
	Runtime    string    `json:"runtime"`
	Status     Status    `json:"status"`
	TTL        int64     `json:"ttl"`
	CreatedAt  time.Time `json:"created_at"`
	Port       int32     `json:"port"`
	PreviewURL string    `json:"preview_url"`
	Image      string    `json:"image"`
}
