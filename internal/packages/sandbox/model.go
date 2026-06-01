package sandbox

import "time"

type Status string

const (
	StatusPending        Status = "pending"
	StatusScheduling     Status = "scheduling"
	StatusAssigned       Status = "assigned"
	StatusStarting       Status = "starting"
	StatusRunning        Status = "running"
	StatusStopped        Status = "stopped"
	StatusFailed         Status = "failed"
	StatusCleanupPending Status = "cleanup_pending"
	StatusCleanedUp      Status = "cleaned_up"
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
	WorkerID   *string   `json:"worker_id"`
}
