package worker

import (
	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	"sync"
	"time"
)

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

// HeartbeatParams carries all fields sent in a worker heartbeat, including
// runtime metrics that are unexported on Worker. Used by the orchestrator to
// construct a fully-populated Worker for upsert without exposing internals.
type HeartbeatParams struct {
	ID        string
	Status    WorkerStatus
	Address   string
	Port      int
	Capacity  int
	CpuBudget int
	MemBudget int
	CpuUsage  int
	MemUsage  int
}

type WorkerService struct {
	orchestratorClient orchestratorv1.OrchestratorServiceClient

	mu     sync.Mutex
	worker Worker
	// map of firecrackerClient *runtime.Client
	runningSandboxes map[string]*runtimeClient.Client

	// map of sandboxes that are currently being deleted, to prevent concurrent deletion attempts
	deletingSandboxes map[string]struct{}
}

type CreateSandboxInput struct {
	ID         string
	Runtime    string
	Image      string
	Port       int32
	TTL        int64
	VcpuCount  int32
	MemSizeMib int32
	PreviewURL string
}

// NewWorkerFromHeartbeat builds a Worker ready for repo.Update / repo.Create
// from heartbeat params received by the orchestrator.
func NewWorkerFromHeartbeat(p HeartbeatParams) Worker {
	return Worker{
		ID:       p.ID,
		Status:   p.Status,
		Address:  p.Address,
		Port:     p.Port,
		Capacity: p.Capacity,
		Budget: Worker_Budget{
			Cpu_budget: p.CpuBudget,
			Mem_budget: p.MemBudget,
		},
		Last_heartbeat: time.Now().UTC(),
		cpu_usage:      p.CpuUsage,
		mem_usage:      p.MemUsage,
	}
}
