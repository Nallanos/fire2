//go:build integration

package divergence

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// LegacyWorker is a worker from before fencing tokens, left running against an
// orchestrator that requires them: its gRPC server answers the orchestrator
// (create, remove, info), but its heartbeats carry no token and no inventory,
// and the orchestrator refuses every one.
type LegacyWorker struct {
	ID string

	server *grpc.Server
	conn   *grpc.ClientConn
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	outcomes map[codes.Code]int // outcome of every heartbeat
}

// AddLegacyWorker starts a legacy worker over vms, serving gRPC on port: the
// one a worker of the same ID listened on, since a legacy worker is the
// previous binary restarted on the same machine.
func (c *Cluster) AddLegacyWorker(id string, vms *FakeVMs, port int) *LegacyWorker {
	c.t.Helper()

	lis, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		c.t.Fatalf("legacy worker %s: listen on port %d: %v", id, port, err)
	}
	conn, err := grpc.NewClient(c.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		c.t.Fatalf("legacy worker %s: dial orchestrator: %v", id, err)
	}
	client := orchestratorv1.NewOrchestratorServiceClient(conn)

	server := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(server, simServer{workerpkg.NewWorkerGRPCServer(workerpkg.NewWorkerService(client, vms))})
	go func() { _ = server.Serve(lis) }()

	ctx, cancel := context.WithCancel(context.Background())
	w := &LegacyWorker{ID: id, server: server, conn: conn, cancel: cancel, outcomes: make(map[codes.Code]int)}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			_, err := client.ReportWorkerHeartbeat(ctx, &orchestratorv1.WorkerHeartbeat{
				WorkerId:  id,
				Status:    "active",
				Address:   "127.0.0.1",
				Port:      int32(port),
				Capacity:  8,
				CpuBudget: 8,
				MemBudget: int32(memBudgetFor(8)),
			})
			w.mu.Lock()
			w.outcomes[status.Code(err)]++
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(heartbeatInterval):
			}
		}
	}()
	c.t.Cleanup(w.Stop)
	return w
}

// Stop shuts the legacy worker down.
func (w *LegacyWorker) Stop() {
	w.cancel()
	w.server.Stop()
	_ = w.conn.Close()
	w.wg.Wait()
}

// Heartbeats returns how many heartbeats ended with code.
func (w *LegacyWorker) Heartbeats(code codes.Code) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.outcomes[code]
}
