package worker

import "time"

type WorkerStatus string

const (
	WorkerStatusActive   WorkerStatus = "active"
	WorkerStatusInactive WorkerStatus = "inactive"
)

type Worker struct {
	ID                string
	Address           string
	Port              int
	Status            WorkerStatus
	Budget            Worker_Budget
	Capacity          int
	Last_heartbeat    time.Time
	running_sandboxes int
	cpu_usage         int
	mem_usage         int
}

type Worker_Budget struct {
	Cpu_budget int // theoretical CPU budget in number of cores, not actual CPU usage, used for scheduling decisions
	Mem_budget int // theoretical memory budget in MB, not actual memory usage, used for scheduling decisions
}
