//go:build integration

package divergence

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// What one sandbox costs a worker (see worker.computeCapacity): the capacity a
// SimWorker advertises is the scarcer of CPU and memory, and the harness
// asserts the orchestrator ends up with the capacity asked for, so a change to
// these numbers fails loudly instead of skewing a scenario.
const (
	sandboxMemMiB      = 256 + 64 // guest memory plus the Firecracker process
	workerMemReserveMi = 384
)

func memBudgetFor(capacity int) int { return workerMemReserveMi + capacity*sandboxMemMiB }

var errPartitioned = status.Error(codes.Unavailable, "network partition")

// SimWorker is a worker machine: the real worker code (registration, fencing
// tokens, heartbeats with their inventory, event reporter, gRPC server) over
// FakeVMs instead of Firecracker.
//
// Its lifecycle controls act on the process, not on the VMs:
//   - Crash stops the process and leaves the VMs running, as the shims do;
//   - Restart starts a new process on the same VMs: a new WorkerService, so a
//     new registration and fencing token, and a new gRPC port;
//   - KillMachine is Crash and the loss of the VMs.
//
// The network controls (Partition, LoseCreateResponses, StripInventory) sit
// between the worker and the orchestrator, in both directions.
type SimWorker struct {
	ID  string
	VMs *FakeVMs

	c        *Cluster
	interval time.Duration

	partitioned    atomic.Bool
	stripInventory atomic.Bool
	lostCreates    atomic.Int64
	createDelay    atomic.Int64 // nanoseconds

	mu       sync.Mutex
	capacity int
	inc      *incarnation // nil while the process is down
	lastPort int
	hbCodes  map[codes.Code]int // outcome of every ReportWorkerHeartbeat
}

// incarnation is one run of the worker process.
type incarnation struct {
	svc    *workerpkg.WorkerService
	server *grpc.Server
	conn   *grpc.ClientConn
	port   int
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// WorkerOption configures AddWorker.
type WorkerOption func(*SimWorker)

// WithCapacity sets how many sandboxes the worker can host (default 8).
func WithCapacity(n int) WorkerOption { return func(w *SimWorker) { w.capacity = n } }

// WithVMs makes the worker run over existing VMs instead of an empty machine.
func WithVMs(vms *FakeVMs) WorkerOption { return func(w *SimWorker) { w.VMs = vms } }

func newSimWorker(c *Cluster, id string, opts ...WorkerOption) *SimWorker {
	w := &SimWorker{
		ID:       id,
		VMs:      NewFakeVMs(),
		c:        c,
		interval: heartbeatInterval,
		capacity: 8,
		hbCodes:  make(map[codes.Code]int),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// start launches a new process: it binds a port, registers and heartbeats.
func (w *SimWorker) start() {
	t := w.c.t
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("worker %s: listen: %v", w.ID, err)
	}
	conn, err := grpc.NewClient(w.c.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("worker %s: dial orchestrator: %v", w.ID, err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	w.VMs.newProcess()
	client := &link{OrchestratorServiceClient: orchestratorv1.NewOrchestratorServiceClient(conn), w: w}
	svc := workerpkg.NewWorkerService(client, w.VMs)
	svc.SetWorkerIdentity(w.ID, "127.0.0.1")
	w.mu.Lock()
	capacity := w.capacity
	w.mu.Unlock()
	svc.SetWorkerBudget(capacity, memBudgetFor(capacity))
	svc.SetListenPort(port)

	server := grpc.NewServer(grpc.UnaryInterceptor(w.intercept))
	workerv1.RegisterWorkerServiceServer(server, simServer{workerpkg.NewWorkerGRPCServer(svc)})
	go func() { _ = server.Serve(lis) }()

	ctx, cancel := context.WithCancel(context.Background())
	inc := &incarnation{svc: svc, server: server, conn: conn, port: port, cancel: cancel}
	inc.wg.Add(2)
	go func() {
		defer inc.wg.Done()
		_ = svc.RunHeartbeat(ctx, w.interval)
	}()
	go func() {
		defer inc.wg.Done()
		workerpkg.NewEventReporter(client, w.ID, svc).Run(ctx)
	}()

	w.mu.Lock()
	w.inc = inc
	w.lastPort = port
	w.mu.Unlock()
}

// Crash stops the worker process. The VMs keep running.
func (w *SimWorker) Crash() {
	w.mu.Lock()
	inc := w.inc
	w.inc = nil
	w.mu.Unlock()
	if inc == nil {
		return
	}
	inc.cancel()
	inc.server.Stop()
	_ = inc.conn.Close()
	inc.wg.Wait()
}

// Restart starts a new process on the machine's surviving VMs. It does not
// wait for the registration: use Cluster.WaitSynced.
func (w *SimWorker) Restart() {
	w.c.t.Helper()
	if w.Running() {
		w.c.t.Fatalf("worker %s: Restart of a process that is still running", w.ID)
	}
	w.start()
}

// KillMachine is Crash, and the VMs are lost with the machine.
func (w *SimWorker) KillMachine() {
	w.Crash()
	w.VMs.Wipe()
}

// Running reports whether the worker process is up.
func (w *SimWorker) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inc != nil
}

// Port is the gRPC port of the current process, or of the last one if it is down.
func (w *SimWorker) Port() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastPort
}

// Partition cuts the network between the worker and the orchestrator in both
// directions: its calls fail with Unavailable, and so do the orchestrator's
// calls to it.
func (w *SimWorker) Partition(on bool) { w.partitioned.Store(on) }

// LoseCreateResponses makes the next n CreateSandbox calls run on the worker
// and then fail with DeadlineExceeded: the VM exists, the orchestrator never
// learns it.
func (w *SimWorker) LoseCreateResponses(n int) { w.lostCreates.Store(int64(n)) }

// SlowCreates makes the worker take d before it handles a CreateSandbox call:
// a loaded machine, or a slow disk. The orchestrator waits that long on each
// attempt.
func (w *SimWorker) SlowCreates(d time.Duration) { w.createDelay.Store(int64(d)) }

// LoseNextCreateResponse is LoseCreateResponses(1).
func (w *SimWorker) LoseNextCreateResponse() { w.LoseCreateResponses(1) }

// StripInventory makes the worker's heartbeats go out without an inventory
// (inventory_complete=false): what a worker that could not list its sandboxes
// sends.
func (w *SimWorker) StripInventory(on bool) { w.stripInventory.Store(on) }

// SetCapacity changes the capacity the worker advertises from its next heartbeat.
func (w *SimWorker) SetCapacity(n int) {
	w.mu.Lock()
	w.capacity = n
	inc := w.inc
	w.mu.Unlock()
	if inc != nil {
		inc.svc.SetWorkerBudget(n, memBudgetFor(n))
	}
}

// Heartbeats returns how many ReportWorkerHeartbeat calls ended with code.
func (w *SimWorker) Heartbeats(code codes.Code) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hbCodes[code]
}

func (w *SimWorker) recordHeartbeat(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.hbCodes[status.Code(err)]++
}

// intercept is the worker's side of the network: it cuts the orchestrator's
// calls during a partition, and loses CreateSandbox responses on demand.
func (w *SimWorker) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if w.partitioned.Load() {
		return nil, errPartitioned
	}
	if d := time.Duration(w.createDelay.Load()); d > 0 && strings.HasSuffix(info.FullMethod, "/CreateSandbox") {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	resp, err := handler(ctx, req)
	if err == nil && strings.HasSuffix(info.FullMethod, "/CreateSandbox") && w.takeLostCreate() {
		return nil, status.Error(codes.DeadlineExceeded, "response lost")
	}
	return resp, err
}

func (w *SimWorker) takeLostCreate() bool {
	for {
		n := w.lostCreates.Load()
		if n <= 0 {
			return false
		}
		if w.lostCreates.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

// link is the worker's client to the orchestrator, with the network between.
type link struct {
	orchestratorv1.OrchestratorServiceClient
	w *SimWorker
}

func (l *link) RegisterWorker(ctx context.Context, in *orchestratorv1.WorkerHeartbeat, opts ...grpc.CallOption) (*orchestratorv1.WorkerRegistration, error) {
	if l.w.partitioned.Load() {
		return nil, errPartitioned
	}
	return l.OrchestratorServiceClient.RegisterWorker(ctx, in, opts...)
}

func (l *link) ReportWorkerHeartbeat(ctx context.Context, in *orchestratorv1.WorkerHeartbeat, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	if l.w.partitioned.Load() {
		return nil, errPartitioned
	}
	if l.w.stripInventory.Load() {
		in.InventoryComplete = false
		in.Sandboxes = nil
	}
	out, err := l.OrchestratorServiceClient.ReportWorkerHeartbeat(ctx, in, opts...)
	l.w.recordHeartbeat(err)
	return out, err
}

func (l *link) IngestSandboxEvent(ctx context.Context, in *orchestratorv1.SandboxEvent, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	if l.w.partitioned.Load() {
		return nil, errPartitioned
	}
	return l.OrchestratorServiceClient.IngestSandboxEvent(ctx, in, opts...)
}

// simServer is the worker's gRPC server, but for the load: the host's CPU and
// memory usage would otherwise decide, from one run to the next, whether the
// scheduler finds a worker "full".
type simServer struct{ *workerpkg.WorkerGRPCServer }

func (s simServer) GetWorkerInfo(ctx context.Context, req *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	resp, err := s.WorkerGRPCServer.GetWorkerInfo(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.CpuUsage, resp.MemUsage = 0, 0
	return resp, nil
}
