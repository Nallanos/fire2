package worker

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"

	"github/nallanos/fire2/internal/packages/docker"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

const defaultHeartbeatInterval = 5 * time.Second
const heartbeatRequestTimeout = 3 * time.Second

type WorkerService struct {
	orchestratorClient orchestratorv1.OrchestratorServiceClient
	sandboxSvc         *sandboxpkg.Service

	mu               sync.Mutex
	worker           Worker
	runningSandboxes map[string]struct{} // keyed by sandbox ID; set is the source of truth for capacity
}

type CreateSandboxInput struct {
	ID         string
	Runtime    string
	Image      string
	Port       int32
	TTL        int64
	PreviewURL string
}

func NewWorkerService(dockerClient docker.ClientInterface, orchestratorClient orchestratorv1.OrchestratorServiceClient) *WorkerService {
	return &WorkerService{
		orchestratorClient: orchestratorClient,
		sandboxSvc:         sandboxpkg.NewRuntimeService(nil, dockerClient),
		runningSandboxes:   make(map[string]struct{}),
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

// SetListenPort records the actual TCP port the worker's gRPC server bound to.
// With ephemeral binding (:0) the OS assigns the port at listen time, so this
// must be called once after the listener is created and before the heartbeat
// loop starts. The reported port flows to the orchestrator via the heartbeat.
func (w *WorkerService) SetListenPort(port int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.worker.Port = port
}

// CreateSandbox starts a container for the sandbox. It is idempotent: if a container
// with this sandbox ID already exists it is reused; duplicate calls for the same ID
// don't increment the running set or capacity count.
func (w *WorkerService) CreateSandbox(ctx context.Context, in CreateSandboxInput) error {
	w.mu.Lock()
	if _, alreadyRunning := w.runningSandboxes[in.ID]; alreadyRunning {
		w.mu.Unlock()
		return nil // idempotent: already tracking this sandbox
	}
	if len(w.runningSandboxes) >= w.worker.Capacity && w.worker.Capacity > 0 {
		log.Printf("worker at capacity: running=%d capacity=%d", len(w.runningSandboxes), w.worker.Capacity)
		w.mu.Unlock()
		return errors.New("worker at full capacity")
	}
	w.runningSandboxes[in.ID] = struct{}{}
	w.mu.Unlock()

	if err := w.sandboxSvc.CreateAndStart(ctx, sandboxpkg.RuntimeCreateRequest{
		ID:         in.ID,
		Runtime:    in.Runtime,
		Image:      in.Image,
		Port:       in.Port,
		TTL:        in.TTL,
		PreviewURL: in.PreviewURL,
	}); err != nil {
		log.Printf("create sandbox failed: id=%s err=%v", in.ID, err)
		w.mu.Lock()
		delete(w.runningSandboxes, in.ID)
		w.mu.Unlock()
		return err
	}

	return nil
}

func (w *WorkerService) StopSandbox(ctx context.Context, containerID string) error {
	return w.sandboxSvc.Stop(ctx, containerID)
}

func (w *WorkerService) RemoveSandbox(ctx context.Context, sandboxID string) error {
	if err := w.sandboxSvc.Remove(ctx, sandboxID); err != nil {
		return err
	}

	w.mu.Lock()
	delete(w.runningSandboxes, sandboxID)
	w.mu.Unlock()

	return nil
}

func (w *WorkerService) GetWorkerInfo(ctx context.Context) (Worker, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.worker, nil
}

// RunHeartbeat periodically updates this worker's heartbeat and runtime metrics in the database.
func (w *WorkerService) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	w.sendHeartbeat(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sendHeartbeat(ctx)
		}
	}
}

func (w *WorkerService) sendHeartbeat(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout)
	defer cancel()
	if _, err := w.UpdateWorker(hctx); err != nil {
		log.Printf("worker heartbeat update failed: %v", err)
	}
}

// UpdateWorker updates the worker's status, resource usage, and heartbeat timestamp in the database.
func (w *WorkerService) UpdateWorker(ctx context.Context) (string, error) {
	cpuUsage := readCPUUsagePercent()
	memUsage := readMemUsageMB()
	address, err := detectWorkerAddress()
	if err != nil {
		address = "127.0.0.1"
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}

	w.mu.Lock()
	if w.worker.ID == "" {
		w.worker.ID = hostname
	}
	if w.worker.Budget.Cpu_budget <= 0 {
		w.worker.Budget.Cpu_budget = runtime.NumCPU()
		if w.worker.Budget.Cpu_budget < 1 {
			w.worker.Budget.Cpu_budget = 1
		}
	}
	if w.worker.Budget.Mem_budget <= 0 {
		w.worker.Budget.Mem_budget = readMemBudgetMB()
	}
	if w.worker.Address == "" {
		w.worker.Address = address
	}
	w.worker.Capacity = w.worker.Budget.Cpu_budget
	w.worker.cpu_usage = cpuUsage
	w.worker.mem_usage = memUsage
	w.worker.Status = WorkerStatusActive
	w.worker.Last_heartbeat = time.Now().UTC()
	snap := w.worker
	w.mu.Unlock()

	_, err = w.orchestratorClient.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
		WorkerId:  snap.ID,
		Status:    string(snap.Status),
		Address:   snap.Address,
		Port:      int32(snap.Port),
		Capacity:  int32(snap.Capacity),
		CpuBudget: int32(snap.Budget.Cpu_budget),
		MemBudget: int32(snap.Budget.Mem_budget),
		CpuUsage:  int32(snap.cpu_usage),
		MemUsage:  int32(snap.mem_usage),
	})
	if err != nil {
		return "", err
	}

	return snap.ID, nil
}

func detectWorkerAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1", err
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
	}
	return "", errors.New("no suitable IP address found")
}

func readMemBudgetMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0
			}
			return kb / 1024
		}
	}
	return 0
}

func readCPUUsagePercent() int {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	cpuCount := runtime.NumCPU()
	if cpuCount < 1 {
		cpuCount = 1
	}
	usage := int((load1 / float64(cpuCount)) * 100)
	if usage < 0 {
		return 0
	}
	if usage > 100 {
		return 100
	}
	return usage
}

func readMemUsageMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	vals := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		vals[key] = v
	}
	total, okT := vals["MemTotal"]
	avail, okA := vals["MemAvailable"]
	if !okT || !okA {
		return 0
	}
	used := total - avail
	if used < 0 {
		return 0
	}
	return used / 1024
}

