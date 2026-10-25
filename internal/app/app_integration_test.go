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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/packages/orchestrator"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

func TestSandboxAPIFlow(t *testing.T) {
	ctx := context.Background()
	sqlDB, pool, cleanup := setupPostgresWithPool(t, ctx)
	defer cleanup()

	if err := applyMigrations(sqlDB); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	workerAddr, workerPort := startFakeWorker(t)
	defer workerAddr.stop()

	workerRepo := workerpkg.NewPostgresRepository(pool)
	_, err := workerRepo.Create(ctx, workerpkg.Worker{
		ID:      "worker-1",
		Status:  workerpkg.WorkerStatusActive,
		Address: workerAddr.host,
		Port:    workerPort,
		Budget: workerpkg.Worker_Budget{
			Cpu_budget: 4,
			Mem_budget: 4096,
		},
		Capacity:       4,
		Last_heartbeat: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}

	riverClient := setupFastRiverClient(t, ctx, pool)

	cfg := Config{Port: "0", DatabaseURL: ""}
	app := New(cfg, pool, riverClient)

	srv := httptest.NewServer(app.Router())
	defer srv.Close()

	auth := testutil.AuthHeader(t, pool)
	created := createSandbox(t, srv.URL, auth)
	if created.ID == "" {
		t.Fatalf("expected sandbox id")
	}

	list := listSandboxes(t, srv.URL, auth)
	if len(list) == 0 {
		t.Fatalf("expected at least one sandbox")
	}

	fetched := getSandbox(t, srv.URL, auth, created.ID)
	if fetched.ID != created.ID {
		t.Fatalf("expected sandbox %s, got %s", created.ID, fetched.ID)
	}
}

func setupFastRiverClient(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()

	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("rivermigrate.New: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("river migrate up: %v", err)
	}

	sandboxRepo := sandboxpkg.NewPostgresRepository(pool)
	workerRepo := workerpkg.NewPostgresRepository(pool)

	workers := river.NewWorkers()
	river.AddWorker(workers, orchestrator.NewCreateSandboxWorker(pool, sandboxRepo, workerRepo))
	river.AddWorker(workers, orchestrator.NewCleanupSandboxWorker(sandboxRepo, workerRepo))

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
			"cleanup":          {MaxWorkers: 5},
		},
		Workers:     workers,
		MaxAttempts: 5,
		RetryPolicy: &fastRetryPolicy{},
	})
	if err != nil {
		t.Fatalf("river.NewClient: %v", err)
	}

	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("riverClient.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	})

	return riverClient
}

type fastRetryPolicy struct{}

func (p *fastRetryPolicy) NextRetry(_ *rivertype.JobRow) time.Time {
	return time.Now().Add(50 * time.Millisecond)
}

func setupPostgresWithPool(t *testing.T, ctx context.Context) (*sql.DB, *pgxpool.Pool, func()) {
	t.Helper()

	container, err := postgres.RunContainer(ctx,
		postgres.WithDatabase("fire2"),
		postgres.WithUsername("fire2"),
		postgres.WithPassword("fire2"),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("pgxpool.New: %v", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	if err := waitForDB(ctx, sqlDB, 20*time.Second); err != nil {
		_ = sqlDB.Close()
		pool.Close()
		_ = container.Terminate(ctx)
		t.Fatalf("wait for db: %v", err)
	}

	cleanup := func() {
		_ = sqlDB.Close()
		pool.Close()
		_ = container.Terminate(ctx)
	}

	return sqlDB, pool, cleanup
}

func setupPostgres(t *testing.T, ctx context.Context) (*sql.DB, func()) {
	sqlDB, _, cleanup := setupPostgresWithPool(t, ctx)
	return sqlDB, cleanup
}

func applyMigrations(sqlDB *sql.DB) error {
	migrationsDir := filepath.Join("..", "db", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		path := filepath.Join(migrationsDir, entry.Name())
		sqlBytes, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sqlText := migrationUpSQL(strings.TrimSpace(string(sqlBytes)))
		if sqlText == "" {
			continue
		}

		if _, err := sqlDB.Exec(sqlText); err != nil {
			return err
		}
	}

	return nil
}

func waitForDB(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		err := db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}

	return lastErr
}

func migrationUpSQL(sqlText string) string {
	const upMarker = "-- migrate:up"
	const downMarker = "-- migrate:down"

	upIndex := strings.Index(sqlText, upMarker)
	if upIndex == -1 {
		return strings.TrimSpace(sqlText)
	}

	upSection := sqlText[upIndex+len(upMarker):]
	downIndex := strings.Index(upSection, downMarker)
	if downIndex != -1 {
		upSection = upSection[:downIndex]
	}

	return strings.TrimSpace(upSection)
}

type fakeWorkerAddress struct {
	host   string
	server *grpc.Server
	lis    net.Listener
}

func (f fakeWorkerAddress) stop() {
	f.server.Stop()
	_ = f.lis.Close()
}

func startFakeWorker(t *testing.T) (fakeWorkerAddress, int) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(grpcServer, &fakeWorkerGRPCServer{})

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	addr := lis.Addr().(*net.TCPAddr)
	return fakeWorkerAddress{host: addr.IP.String(), server: grpcServer, lis: lis}, addr.Port
}

type fakeWorkerGRPCServer struct {
	workerv1.UnimplementedWorkerServiceServer

	mu      sync.Mutex
	created []string
}

func (f *fakeWorkerGRPCServer) CreateSandbox(_ context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req.GetId())
	return &workerv1.CreateSandboxResponse{}, nil
}

func (f *fakeWorkerGRPCServer) ListRunningSandboxes(_ context.Context, _ *workerv1.ListRunningSandboxesRequest) (*workerv1.ListRunningSandboxesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &workerv1.ListRunningSandboxesResponse{ContainerIds: append([]string(nil), f.created...)}, nil
}

func (f *fakeWorkerGRPCServer) GetWorkerInfo(_ context.Context, _ *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	return &workerv1.GetWorkerInfoResponse{
		Id:        "worker-1",
		Address:   "127.0.0.1",
		Status:    "active",
		CpuBudget: 4,
		MemBudget: 4096,
		Capacity:  4,
		CpuUsage:  0,
		MemUsage:  0,
	}, nil
}

// doRequest sends a request carrying the Authorization header auth. The caller
// closes the response body.
func doRequest(t *testing.T, method, url, auth string, body []byte) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// postSandbox sends body to POST /api/sandboxes as the user behind auth.
func postSandbox(t *testing.T, baseURL, auth string, body []byte) *http.Response {
	t.Helper()
	return doRequest(t, http.MethodPost, baseURL+"/api/sandboxes", auth, body)
}

func createSandbox(t *testing.T, baseURL, auth string) sandboxResponse {
	t.Helper()

	payload := map[string]any{
		"runtime": "node",
		"image":   "node:20-alpine",
		"port":    10001,
		"ttl":     3600,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	resp := postSandbox(t, baseURL, auth, body)
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

func listSandboxes(t *testing.T, baseURL, auth string) []sandboxResponse {
	t.Helper()

	resp := doRequest(t, http.MethodGet, baseURL+"/api/sandboxes", auth, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var items []sandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return items
}

func getSandbox(t *testing.T, baseURL, auth, id string) sandboxResponse {
	t.Helper()

	resp := doRequest(t, http.MethodGet, baseURL+"/api/sandboxes/"+id, auth, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var sbx sandboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&sbx); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return sbx
}

type sandboxResponse struct {
	ID         string          `json:"id"`
	Runtime    string          `json:"runtime"`
	Status     string          `json:"status"`
	TTL        int64           `json:"ttl"`
	CreatedAt  json.RawMessage `json:"created_at"`
	Port       int32           `json:"port"`
	PreviewURL string          `json:"preview_url"`
	Image      string          `json:"image"`
	WorkerID   *string         `json:"worker_id"`
}
