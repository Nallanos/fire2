//go:build integration

package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
	"google.golang.org/grpc"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	workerpb "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/docker"
	"github/nallanos/fire2/internal/packages/orchestrator"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

func TestSandboxFlowWithMultipleWorkers(t *testing.T) {
	ctx := context.Background()
	sqlDB, cleanup := setupPostgres(t, ctx)
	defer cleanup()

	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	queries := db.New(sqlDB)
	grpcAddr, stopGRPC := startOrchestratorGRPC(t, queries)
	defer stopGRPC()

	dockerClient, err := docker.NewClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	workerAddr, workerPort, stopWorker := startWorkerServer(t, dockerClient, queries, "worker-a", 8, 4, 4096)
	defer stopWorker()

	if err := registerWorker(ctx, queries, "worker-a", workerAddr, workerPort, 8, 4, 4096); err != nil {
		t.Fatalf("register worker-a: %v", err)
	}
	if err := registerWorker(ctx, queries, "worker-b", workerAddr, workerPort, 4, 2, 2048); err != nil {
		t.Fatalf("register worker-b: %v", err)
	}

	eventClient, err := orchestrator.NewEventClient(ctx, grpcAddr)
	if err != nil {
		t.Fatalf("event client: %v", err)
	}
	defer eventClient.Close()

	reporter := workerpkg.NewEventReporter(dockerClient, eventClient.Client(), "worker-a")
	reporterCtx, cancelReporter := context.WithCancel(ctx)
	defer cancelReporter()
	go reporter.Run(reporterCtx)

	cfg := Config{Port: "0", DatabaseURL: ""}
	app := New(cfg, sqlDB)
	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	sandboxes := []sandboxResponse{
		createSandboxWithImage(t, srv.URL, "node", "node:20-alpine"),
		createSandboxWithImage(t, srv.URL, "python", "python:3.12-alpine"),
	}

	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker sdk client: %v", err)
	}
	defer cli.Close()

	for _, sbx := range sandboxes {
		if sbx.ID == "" {
			t.Fatalf("expected sandbox id")
		}

		containerID, err := waitForContainerByLabel(ctx, cli, "sandbox_id", sbx.ID, 20*time.Second)
		if err != nil {
			t.Fatalf("sandbox container not found: %v", err)
		}
		defer removeContainer(ctx, cli, containerID)

		if err := waitForSandboxEvents(ctx, sqlDB, sbx.ID, 1, 20*time.Second); err != nil {
			t.Fatalf("sandbox events missing for %s: %v", sbx.ID, err)
		}
	}
}

func startOrchestratorGRPC(t *testing.T, queries *db.Queries) (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("orchestrator listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	orchestratorv1.RegisterOrchestratorServiceServer(grpcServer, orchestrator.NewEventGRPCServer(queries))

	go func() {
		_ = grpcServer.Serve(listener)
	}()

	cleanup := func() {
		grpcServer.Stop()
		_ = listener.Close()
	}

	return listener.Addr().String(), cleanup
}

func startWorkerServer(t *testing.T, dockerClient docker.ClientInterface, queries *db.Queries, workerID string, capacity, cpuBudget, memBudget int) (string, int, func()) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("worker listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	workerService := workerpkg.NewWorkerService(dockerClient, queries)
	setWorkerInfo(t, workerService, workerpkg.Worker{
		ID:       workerID,
		Address:  "127.0.0.1",
		Port:     port,
		Status:   workerpkg.WorkerStatusActive,
		Capacity: capacity,
		Budget: workerpkg.Worker_Budget{
			Cpu_budget: cpuBudget,
			Mem_budget: memBudget,
		},
		Last_heartbeat: time.Now().UTC(),
	})

	grpcServer := grpc.NewServer()
	workerpb.RegisterWorkerServiceServer(grpcServer, workerpkg.NewWorkerGRPCServer(workerService))

	go func() {
		_ = grpcServer.Serve(listener)
	}()

	cleanup := func() {
		grpcServer.Stop()
		_ = listener.Close()
	}

	return "127.0.0.1", port, cleanup
}

func setWorkerInfo(t *testing.T, svc *workerpkg.WorkerService, info workerpkg.Worker) {
	v := reflect.ValueOf(svc).Elem().FieldByName("worker")
	if !v.IsValid() {
		t.Fatalf("worker field not found")
	}

	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(info))
}

func registerWorker(ctx context.Context, queries *db.Queries, id, address string, port, capacity, cpuBudget, memBudget int) error {
	_, err := queries.CreateWorker(ctx, db.CreateWorkerParams{
		ID:            id,
		Status:        "active",
		Address:       address,
		Capacity:      int32(capacity),
		Port:          int32(port),
		CpuBudget:     int32(cpuBudget),
		MemBudget:     int32(memBudget),
		CpuUsage:      0,
		MemUsage:      0,
		LastHeartbeat: time.Now().UTC(),
		CreatedAt:     time.Now().UTC(),
	})
	return err
}

func createSandboxWithImage(t *testing.T, baseURL, runtime, image string) sandboxResponse {
	payload := map[string]any{
		"runtime": runtime,
		"image":   image,
		"port":    10000 + (time.Now().UnixNano() % 50000),
		"ttl":     3600,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp, err := http.Post(baseURL+"/api/sandboxes", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post sandbox: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", resp.StatusCode)
	}

	var sbx sandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbx); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return sbx
}

func waitForContainerByLabel(ctx context.Context, cli *dockerclient.Client, labelKey, labelValue string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		filterArgs := filters.NewArgs()
		filterArgs.Add("label", labelKey+"="+labelValue)
		containers, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filterArgs})
		if err == nil && len(containers) > 0 {
			return containers[0].ID, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return "", context.DeadlineExceeded
}

func removeContainer(ctx context.Context, cli *dockerclient.Client, containerID string) {
	if containerID == "" {
		return
	}
	_ = cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

func waitForSandboxEvents(ctx context.Context, dbConn *sql.DB, sandboxID string, minCount int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		var count int
		if err := dbConn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sandbox_events WHERE sandbox_id = $1", sandboxID).Scan(&count); err == nil && count >= minCount {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return context.DeadlineExceeded
}
