package worker

import (
	"context"
	"errors"
	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

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

func (w *WorkerService) GetWorkerInfo(ctx context.Context) (Worker, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.worker, nil
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
