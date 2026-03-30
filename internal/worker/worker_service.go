package worker

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/docker"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

const defaultWorkerPort = 50051
const defaultHeartbeatInterval = 5 * time.Second
const heartbeatRequestTimeout = 3 * time.Second

type WorkerService struct {
	db          db.Querier
	sandboxRepo sandboxpkg.Repository
	sandboxSvc  *sandboxpkg.Service
	worker      Worker

	mu sync.Mutex // protects access to active count
}

type CreateSandboxInput struct {
	ID         string
	Runtime    string
	Image      string
	Port       int32
	TTL        int64
	PreviewURL string
}

func NewWorkerService(docker docker.ClientInterface, db db.Querier) *WorkerService {
	repo := sandboxpkg.NewPostgresRepository(db)
	return &WorkerService{
		db:          db,
		sandboxRepo: repo,
		sandboxSvc:  sandboxpkg.NewRuntimeService(repo, docker),
	}
}

func (w *WorkerService) CreateSandbox(ctx context.Context, in CreateSandboxInput) (db.Sandbox, error) {
	// Check if worker has capacity to handle new sandbox
	w.mu.Lock()
	if w.worker.running_sandboxes >= w.worker.Capacity {
		w.mu.Unlock()
		return db.Sandbox{}, errors.New("worker at full capacity")
	}
	w.worker.running_sandboxes++
	w.mu.Unlock()

	sandbox, err := w.sandboxSvc.CreateAndStart(ctx, sandboxpkg.RuntimeCreateRequest{
		ID:         in.ID,
		Runtime:    in.Runtime,
		Image:      in.Image,
		Port:       in.Port,
		TTL:        in.TTL,
		PreviewURL: in.PreviewURL,
	})
	if err != nil {
		w.mu.Lock()
		w.worker.running_sandboxes--
		w.mu.Unlock()
		return db.Sandbox{}, err
	}

	return db.Sandbox{
		ID:         sandbox.ID,
		Runtime:    sandbox.Runtime,
		Status:     string(sandbox.Status),
		Ttl:        sandbox.TTL,
		CreatedAt:  sandbox.CreatedAt,
		Port:       sandbox.Port,
		PreviewUrl: sandbox.PreviewURL,
		Image:      sandbox.Image,
	}, nil
}

func (w *WorkerService) StopSandbox(ctx context.Context, containerID string) error {
	return w.sandboxSvc.Stop(ctx, containerID)
}

func (w *WorkerService) RemoveSandbox(ctx context.Context, containerID string) error {
	err := w.sandboxSvc.Remove(ctx, containerID)
	if err != nil {
		return err
	}

	w.mu.Lock()
	if w.worker.running_sandboxes > 0 {
		w.worker.running_sandboxes--
	}
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
	heartbeatCtx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout)
	defer cancel()

	if _, err := w.UpdateWorker(heartbeatCtx); err != nil {
		log.Printf("worker heartbeat update failed: %v", err)
	}
}

// UpdateWorker updates the worker's status, resource usage, and heartbeat timestamp in the database. It returns the worker ID or an error if the update fails.
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

	// Budget is theoretical and should stay stable once initialized.
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
	if w.worker.Port <= 0 {
		w.worker.Port = readWorkerPort()
	}
	w.worker.Capacity = w.worker.Budget.Cpu_budget
	w.worker.cpu_usage = cpuUsage
	w.worker.mem_usage = memUsage
	w.worker.Status = WorkerStatusActive
	w.worker.Last_heartbeat = time.Now().UTC()
	workerSnapshot := w.worker
	w.mu.Unlock()

	_, err = w.db.UpdateWorker(ctx, db.UpdateWorkerParams{
		ID:       workerSnapshot.ID,
		Status:   string(workerSnapshot.Status),
		Address:  workerSnapshot.Address,
		Capacity: int32(workerSnapshot.Capacity),
		Port:     int32(workerSnapshot.Port),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, createErr := w.db.CreateWorker(ctx, db.CreateWorkerParams{
				ID:        workerSnapshot.ID,
				Status:    string(workerSnapshot.Status),
				Address:   workerSnapshot.Address,
				Capacity:  int32(workerSnapshot.Capacity),
				Port:      int32(workerSnapshot.Port),
				CreatedAt: workerSnapshot.Last_heartbeat,
			})
			if createErr != nil {
				return "", createErr
			}

			return workerSnapshot.ID, nil
		}

		return "", err
	}

	return workerSnapshot.ID, nil
}

// detectWorkerAddress attempts to find a non-loopback IPv4 address for the worker. If it fails, it defaults to "127.0.0.1" and returns the error.
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

		ipv4 := ipNet.IP.To4()
		if ipv4 != nil {
			return ipv4.String(), nil
		}
	}

	return "", errors.New("no suitable IP address found")
}

// readMemBudgetMB reads the total memory available on the system in megabytes by parsing /proc/meminfo.
func readMemBudgetMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}

			totalKB, convErr := strconv.Atoi(fields[1])
			if convErr != nil {
				return 0
			}

			return totalKB / 1024
		}
	}

	return 0
}

// readCPUUsagePercent reads the 1-minute load average from /proc/loadavg and calculates the CPU usage percentage based on the number of CPU cores. It returns a value between 0 and 100.
func readCPUUsagePercent() int {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}

	load1, convErr := strconv.ParseFloat(fields[0], 64)
	if convErr != nil {
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

// readMemUsageMB reads the current memory usage in megabytes by parsing /proc/meminfo and calculating the difference between total and available memory.
func readMemUsageMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}

	lines := strings.Split(string(data), "\n")
	values := map[string]int{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		key := strings.TrimSuffix(fields[0], ":")
		v, convErr := strconv.Atoi(fields[1])
		if convErr != nil {
			continue
		}
		values[key] = v
	}

	totalKB, okTotal := values["MemTotal"]
	availableKB, okAvail := values["MemAvailable"]
	if !okTotal || !okAvail {
		return 0
	}

	usedKB := totalKB - availableKB
	if usedKB < 0 {
		usedKB = 0
	}

	usedMB := usedKB / 1024
	if usedMB < 0 {
		return 0
	}

	return usedMB
}

func readWorkerPort() int {
	raw := strings.TrimSpace(os.Getenv("WORKER_PORT"))
	if raw == "" {
		return defaultWorkerPort
	}

	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 {
		return defaultWorkerPort
	}

	return port
}
