//go:build integration

// Package divergence tests the loop that keeps the database true to the
// machines: workers that crash and come back, responses that get lost,
// networks that cut. Each scenario creates a gap on purpose, lets the real
// mechanisms run (reapers, inventory reconciliation, fencing, cleanup jobs),
// then checks the invariant holds again within a bounded delay:
//
//	for every sandbox, the database says assigned|starting|running on worker W
//	if and only if W really hosts its VM.
//
// Both sides run their real code: the orchestrator (Postgres, River jobs, gRPC
// server, HTTP API, wired by internal/app as in production) and the worker
// (WorkerService, EventReporter, gRPC server). Only Firecracker is replaced, by
// FakeVMs, so no root or KVM is needed.
package divergence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/app"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
	"github/nallanos/fire2/internal/testutil"
)

// The delays the orchestrator runs with in a test, in place of production's
// (15s heartbeat timeout, 10s and 1m reapers, 5s worker heartbeats).
const (
	heartbeatInterval     = 100 * time.Millisecond
	heartbeatTimeout      = 2 * time.Second
	reaperInterval        = 200 * time.Millisecond
	livenessTimeout       = 2 * time.Second
	sandboxReaperInterval = 200 * time.Millisecond
	// createTimeout is generous: a healthy creation takes a few seconds, and a
	// scenario that is not about stalled creations must never see one reaped.
	createTimeout = 30 * time.Second

	// settle bounds how long the system gets to converge after a scenario's
	// last action.
	settle = 20 * time.Second
)

// Cluster is an orchestrator on its own Postgres, and the workers added to it.
type Cluster struct {
	t *testing.T

	GRPCAddr string // orchestrator gRPC, where workers register and heartbeat
	API      *httptest.Server
	Pool     *pgxpool.Pool
	River    *river.Client[pgx.Tx]

	auth      string
	http      *http.Client
	sandboxes sandboxpkg.Repository
	workers   workerpkg.Repository

	mu   sync.Mutex
	sims map[string]*SimWorker // the latest worker added under each ID
}

// ClusterOption changes the orchestrator's configuration.
type ClusterOption func(*app.Config)

// WithLivenessTimeout changes how long a worker may go without listing a
// sandbox before it is failed. A scenario that checks one mechanism sets it
// high enough that liveness cannot be what fixed things.
func WithLivenessTimeout(d time.Duration) ClusterOption {
	return func(cfg *app.Config) { cfg.SandboxLivenessTimeout = d }
}

// WithCreateTimeout changes how long a sandbox may stay pending or scheduling
// before the reaper fails it.
func WithCreateTimeout(d time.Duration) ClusterOption {
	return func(cfg *app.Config) { cfg.SandboxCreateTimeout = d }
}

// newCluster starts an orchestrator the way cmd/api wires it, with its own
// database. Scenarios are independent, so it also marks the test parallel.
func newCluster(t *testing.T, opts ...ClusterOption) *Cluster {
	t.Helper()
	t.Parallel()
	ctx := context.Background()

	cfg := app.Config{
		Port:                   "0",
		HeartbeatTimeout:       heartbeatTimeout,
		ReaperInterval:         reaperInterval,
		SandboxLivenessTimeout: livenessTimeout,
		SandboxReaperInterval:  sandboxReaperInterval,
		SandboxCreateTimeout:   createTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	pool := testutil.SetupPostgres(t, ctx)
	testutil.MigrateRiver(t, ctx, pool)

	riverClient, err := app.NewRiverClient(pool, cfg, &testutil.FastRetryPolicy{})
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	if err := riverClient.Start(ctx); err != nil {
		t.Fatalf("start river: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = riverClient.StopAndCancel(stopCtx)
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	orchestratorv1.RegisterOrchestratorServiceServer(grpcServer, app.NewEventServer(pool, riverClient))
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	api := httptest.NewServer(app.New(cfg, pool, riverClient).Router())
	t.Cleanup(api.Close)

	return &Cluster{
		t:         t,
		GRPCAddr:  lis.Addr().String(),
		API:       api,
		Pool:      pool,
		River:     riverClient,
		auth:      testutil.AuthHeader(t, pool),
		http:      &http.Client{Timeout: 60 * time.Second},
		sandboxes: sandboxpkg.NewPostgresRepository(pool),
		workers:   workerpkg.NewPostgresRepository(pool),
		sims:      make(map[string]*SimWorker),
	}
}

// AddWorker starts a worker and waits until the orchestrator has taken its
// first inventory. A second worker added under the same ID is a replacement
// for the first, which keeps running: the old process that was not stopped.
func (c *Cluster) AddWorker(id string, opts ...WorkerOption) *SimWorker {
	c.t.Helper()
	w := newSimWorker(c, id, opts...)
	c.mu.Lock()
	c.sims[id] = w
	c.mu.Unlock()
	c.t.Cleanup(w.Crash)
	w.start()
	c.WaitSynced(w)
	return w
}

// WaitSynced waits until the worker's current process is registered with the
// orchestrator, reachable at its port, and its first inventory was reconciled.
func (c *Cluster) WaitSynced(w *SimWorker) {
	c.t.Helper()
	ok := eventually(c.t, settle, func() bool {
		token, inventoryToken, found := c.tokens(w.ID)
		if !found || token == 0 || inventoryToken != token {
			return false
		}
		row, err := c.workers.Get(context.Background(), w.ID)
		w.mu.Lock()
		capacity := w.capacity
		w.mu.Unlock()
		return err == nil && row.Port == w.Port() && row.Capacity == capacity
	})
	if !ok {
		c.t.Fatalf("worker %s never synced with the orchestrator\n%s", w.ID, c.dump())
	}
}

// tokens reads the fencing token of the worker row and the token whose first
// inventory was reconciled.
func (c *Cluster) tokens(workerID string) (token, inventoryToken int64, found bool) {
	err := c.Pool.QueryRow(context.Background(),
		`SELECT fencing_token, inventory_token FROM worker WHERE id = $1`, workerID).Scan(&token, &inventoryToken)
	return token, inventoryToken, err == nil
}

// TryCreate creates a sandbox through the HTTP API, as a user would, and
// returns the HTTP status with the sandbox of a 201. A zero ttl takes the API's
// default.
func (c *Cluster) TryCreate(ttl time.Duration) (sandboxpkg.Sandbox, int) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"runtime": "node", "ttl": int64(ttl / time.Second)})
	resp := c.do(http.MethodPost, "/api/sandboxes", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, resp.Body)
		return sandboxpkg.Sandbox{}, resp.StatusCode
	}
	var sbx sandboxpkg.Sandbox
	if err := json.NewDecoder(resp.Body).Decode(&sbx); err != nil {
		c.t.Fatalf("decode created sandbox: %v", err)
	}
	return sbx, resp.StatusCode
}

// Create is TryCreate, and fails the test unless the sandbox came up running.
func (c *Cluster) Create() sandboxpkg.Sandbox {
	c.t.Helper()
	sbx, code := c.TryCreate(0)
	if code != http.StatusCreated {
		c.t.Fatalf("create sandbox: HTTP %d, want 201\n%s", code, c.dump())
	}
	if sbx.Status != sandboxpkg.StatusRunning || sbx.WorkerID == nil {
		c.t.Fatalf("create sandbox: got status %s on worker %v, want running on a worker", sbx.Status, sbx.WorkerID)
	}
	return sbx
}

// DeleteSandbox asks for a sandbox to be deleted and returns the HTTP status.
func (c *Cluster) DeleteSandbox(id string) int {
	c.t.Helper()
	resp := c.do(http.MethodDelete, "/api/sandboxes/"+id, nil)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func (c *Cluster) do(method, path string, body []byte) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.API.URL+path, bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", c.auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// SeedSandbox inserts a sandbox row directly, with no River job behind it.
func (c *Cluster) SeedSandbox(status sandboxpkg.Status, ttl time.Duration) sandboxpkg.Sandbox {
	c.t.Helper()
	sbx, err := c.sandboxes.Create(context.Background(), sandboxpkg.Sandbox{
		ID:         uuid.NewString(),
		Runtime:    "node",
		Status:     status,
		Image:      "node:20-alpine",
		Port:       3000,
		TTL:        int64(ttl / time.Second),
		CreatedAt:  time.Now().UTC(),
		VcpuCount:  1,
		MemSizeMib: 256,
	})
	if err != nil {
		c.t.Fatalf("seed sandbox: %v", err)
	}
	return sbx
}

// Sandbox reads a sandbox row.
func (c *Cluster) Sandbox(id string) sandboxpkg.Sandbox {
	c.t.Helper()
	sbx, err := c.sandboxes.GetByID(context.Background(), id)
	if err != nil {
		c.t.Fatalf("read sandbox %s: %v", id, err)
	}
	return sbx
}

// Sandboxes reads every sandbox row.
func (c *Cluster) Sandboxes() []sandboxpkg.Sandbox {
	c.t.Helper()
	rows, err := c.sandboxes.List(context.Background())
	if err != nil {
		c.t.Fatalf("list sandboxes: %v", err)
	}
	return rows
}

// Worker reads a worker row.
func (c *Cluster) Worker(id string) workerpkg.Worker {
	c.t.Helper()
	w, err := c.workers.Get(context.Background(), id)
	if err != nil {
		c.t.Fatalf("read worker %s: %v", id, err)
	}
	return w
}

// Orchestrator returns a bare client of the orchestrator's gRPC service, for a
// scenario that sends by hand what a worker would.
func (c *Cluster) Orchestrator() orchestratorv1.OrchestratorServiceClient {
	c.t.Helper()
	conn, err := grpc.NewClient(c.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		c.t.Fatalf("dial orchestrator: %v", err)
	}
	c.t.Cleanup(func() { _ = conn.Close() })
	return orchestratorv1.NewOrchestratorServiceClient(conn)
}

// WaitStatus waits for a sandbox to reach one of the statuses.
func (c *Cluster) WaitStatus(id string, within time.Duration, want ...sandboxpkg.Status) sandboxpkg.Sandbox {
	c.t.Helper()
	var last sandboxpkg.Sandbox
	ok := eventually(c.t, within, func() bool {
		sbx, err := c.sandboxes.GetByID(context.Background(), id)
		if err != nil {
			return false
		}
		last = sbx
		for _, st := range want {
			if sbx.Status == st {
				return true
			}
		}
		return false
	})
	if !ok {
		c.t.Fatalf("sandbox %s: status %s after %s, want one of %v\n%s", id, last.Status, within, want, c.dump())
	}
	return last
}

// registered returns the workers added to the cluster, by ID.
func (c *Cluster) registered() []*SimWorker {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*SimWorker, 0, len(c.sims))
	for _, w := range c.sims {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// dump describes the whole state, database and machines, for a failure message.
func (c *Cluster) dump() string {
	var b strings.Builder
	ctx := context.Background()

	b.WriteString("database:\n")
	rows, err := c.sandboxes.List(ctx)
	if err != nil {
		fmt.Fprintf(&b, "  cannot list sandboxes: %v\n", err)
	}
	for _, s := range rows {
		worker := "-"
		if s.WorkerID != nil {
			worker = *s.WorkerID
		}
		fmt.Fprintf(&b, "  sandbox %s  %-15s worker=%s\n", short(s.ID), s.Status, worker)
	}
	workers, err := c.workers.List(ctx)
	if err != nil {
		fmt.Fprintf(&b, "  cannot list workers: %v\n", err)
	}
	for _, w := range workers {
		token, inventoryToken, _ := c.tokens(w.ID)
		fmt.Fprintf(&b, "  worker %s  %-8s token=%d inventory_token=%d port=%d capacity=%d last_heartbeat=%s ago\n",
			w.ID, w.Status, token, inventoryToken, w.Port, w.Capacity, time.Since(w.Last_heartbeat).Round(10*time.Millisecond))
	}

	b.WriteString("machines:\n")
	for _, w := range c.registered() {
		var vms []string
		for _, e := range w.VMs.Snapshot() {
			vms = append(vms, fmt.Sprintf("%s:%s", short(e.GetSandboxId()), strings.TrimPrefix(e.GetState().String(), "SANDBOX_STATE_")))
		}
		fmt.Fprintf(&b, "  worker %s  process up=%t  VMs=[%s]\n", w.ID, w.Running(), strings.Join(vms, " "))
	}
	return b.String()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
