package deploy

import (
	"time"
)

type DeploymentStatus string

const (
	DeploymentStatusPending DeploymentStatus = "pending"
	DeploymentStatusRunning DeploymentStatus = "running"
	DeploymentStatusStopped DeploymentStatus = "stopped"
	DeploymentStatusFailed  DeploymentStatus = "failed"
)

type Deployment struct {
	ID        string           `json:"id"`
	BuildID   string           `json:"build_id"`
	Status    DeploymentStatus `json:"status"`
	ImageTag  string           `json:"image_tag"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	Port      string           `json:"port"`
}
