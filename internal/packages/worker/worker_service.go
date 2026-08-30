package worker

import (
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/docker"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
)

const defaultHeartbeatInterval = 5 * time.Second
const heartbeatRequestTimeout = 3 * time.Second

func NewWorkerService(dockerClient docker.ClientInterface, orchestratorClient orchestratorv1.OrchestratorServiceClient) *WorkerService {
	return &WorkerService{
		orchestratorClient: orchestratorClient,
		runningSandboxes:   make(map[string]*runtimeClient.Client),
	}
}

// SetWorkerIdentity pins the worker ID and advertised address so heartbeats
// and event reports use consistent values. Call once at startup before the
// heartbeat loop begins. Empty strings are ignored.
func (w *WorkerService) SetWorkerIdentity(id, address string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id != "" {
		w.worker.ID = id
	}
	if address != "" {
		w.worker.Address = address
	}
}

// SetWorkerBudget pins the CPU and memory budgets so the heartbeat loop does
// not fall back to auto-detecting the full VM resources from /proc. Call once
// at startup before the heartbeat loop begins. Zero values are ignored.
func (w *WorkerService) SetWorkerBudget(cpu, mem int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cpu > 0 {
		w.worker.Budget.Cpu_budget = cpu
	}
	if mem > 0 {
		w.worker.Budget.Mem_budget = mem
	}
}

// SetListenPort records the actual TCP port the worker's gRPC server bound to.
// With ephemeral binding (:0) the OS assigns the port at listen time, so this
// must be called once after the listener is created and before the heartbeat
// loop starts. The reported port flows to the orchestrator via the heartbeat.
func (w *WorkerService) SetListenPort(port int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.worker.Port = port
}
