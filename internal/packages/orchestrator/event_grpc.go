package orchestrator

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

type EventGRPCServer struct {
	orchestratorv1.UnimplementedOrchestratorServiceServer
	sandboxRepo sandboxpkg.Repository
	eventRepo   EventRepository
	workerRepo  workerpkg.Repository

	// pool runs the heartbeat transaction (fencing check + worker update +
	// inventory reconciliation). Required to accept heartbeat inventories.
	pool *pgxpool.Pool
	// riverClient enqueues orphan removals in the heartbeat transaction. When
	// nil, orphans are only logged.
	riverClient *river.Client[pgx.Tx]
}

// EventServerOption configures optional collaborators of EventGRPCServer.
type EventServerOption func(*EventGRPCServer)

// WithTransactions gives the server the pool used to reconcile heartbeat
// inventories atomically.
func WithTransactions(pool *pgxpool.Pool) EventServerOption {
	return func(s *EventGRPCServer) { s.pool = pool }
}

// WithRiverClient lets heartbeat reconciliation enqueue removal of orphan
// sandboxes.
func WithRiverClient(c *river.Client[pgx.Tx]) EventServerOption {
	return func(s *EventGRPCServer) { s.riverClient = c }
}

func NewEventGRPCServer(sandboxRepo sandboxpkg.Repository, eventRepo EventRepository, workerRepo workerpkg.Repository, opts ...EventServerOption) *EventGRPCServer {
	s := &EventGRPCServer{sandboxRepo: sandboxRepo, eventRepo: eventRepo, workerRepo: workerRepo}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// IngestSandboxEvent stores a sandbox event and updates sandbox status.
// State ownership: the create-sandbox job owns transitions pending→running.
// Events own transitions starting→running only — see statusTransitionForState
// for why running/starting→failed isn't handled here anymore.
func (s *EventGRPCServer) IngestSandboxEvent(ctx context.Context, req *orchestratorv1.SandboxEvent) (*emptypb.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing sandbox event")
	}

	eventID := strings.TrimSpace(req.GetId())
	if eventID == "" {
		eventID = uuid.NewString()
	}

	occurredAt := time.Now().UTC()
	if ts := req.GetOccurredAt(); ts != nil {
		occurredAt = ts.AsTime().UTC()
	}

	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if sandboxID == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox_id is required")
	}

	workerID := strings.TrimSpace(req.GetWorkerId())
	state := strings.TrimSpace(req.GetState())
	if workerID == "" || state == "" {
		return nil, status.Error(codes.InvalidArgument, "missing required fields")
	}

	token := req.GetFencingToken()
	if token <= 0 {
		return nil, status.Error(codes.InvalidArgument, "fencing_token is required")
	}
	current, err := s.workerRepo.Get(ctx, workerID)
	if errors.Is(err, workerpkg.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "worker is not registered")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get worker: %v", err)
	}
	if current.FencingToken != token {
		return nil, status.Error(codes.FailedPrecondition, "worker fencing token is stale")
	}

	_, err = s.eventRepo.CreateSandboxEvent(ctx, SandboxEvent{
		ID:         eventID,
		SandboxID:  sandboxID,
		WorkerID:   workerID,
		State:      state,
		OccurredAt: occurredAt,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store sandbox event: %v", err)
	}

	s.applyStatusFromState(ctx, sandboxID, workerID, token, state)

	return &emptypb.Empty{}, nil
}

// RegisterWorker starts a new worker incarnation and returns its fencing token.
// The worker must include this token in all subsequent heartbeats.
func (s *EventGRPCServer) RegisterWorker(ctx context.Context, req *orchestratorv1.WorkerHeartbeat) (*orchestratorv1.WorkerRegistration, error) {
	if req == nil || strings.TrimSpace(req.GetWorkerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	w, err := s.workerRepo.Register(ctx, workerFromHeartbeat(req))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register worker: %v", err)
	}
	return &orchestratorv1.WorkerRegistration{FencingToken: w.FencingToken}, nil
}

// ReportWorkerHeartbeat updates a registered worker only if its fencing token
// still identifies the current incarnation. A stale worker cannot reactivate
// itself or overwrite the replacement's metrics.
func (s *EventGRPCServer) ReportWorkerHeartbeat(ctx context.Context, req *orchestratorv1.WorkerHeartbeat) (*emptypb.Empty, error) {
	if req == nil || strings.TrimSpace(req.GetWorkerId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	if req.GetFencingToken() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "fencing_token is required")
	}

	w := workerFromHeartbeat(req)

	log.Printf("received worker heartbeat: worker_id=%s status=%s address=%s port=%d capacity=%d cpu_usage=%d mem_usage=%d",
		req.GetWorkerId(), req.GetStatus(), req.GetAddress(), req.GetPort(), req.GetCapacity(), req.GetCpuUsage(), req.GetMemUsage())

	if !req.GetInventoryComplete() {
		// No usable inventory: only the worker row is refreshed. Sandbox
		// presence is left untouched, so missing inventories age toward
		// liveness expiry rather than being mistaken for an empty worker.
		if _, err := s.workerRepo.Update(ctx, w); err != nil {
			return nil, heartbeatError(err)
		}
		return &emptypb.Empty{}, nil
	}

	if err := s.reconcileHeartbeat(ctx, w, req.GetSandboxes()); err != nil {
		return nil, heartbeatError(err)
	}
	return &emptypb.Empty{}, nil
}

func heartbeatError(err error) error {
	if errors.Is(err, workerpkg.ErrFenced) {
		return status.Error(codes.FailedPrecondition, "worker fencing token is stale")
	}
	return status.Errorf(codes.Internal, "heartbeat: %v", err)
}

// reconcileHeartbeat verifies the fencing token, refreshes the worker row and
// reconciles the reported inventory against the database in a single
// transaction. A stale token fails the worker update and rolls everything back.
func (s *EventGRPCServer) reconcileHeartbeat(ctx context.Context, w workerpkg.Worker, inventory []*orchestratorv1.SandboxInventoryItem) error {
	if s.pool == nil {
		return errors.New("inventory reconciliation is not configured")
	}

	items := make([]sandboxpkg.InventoryItem, 0, len(inventory))
	seen := make(map[string]struct{}, len(inventory))
	for _, it := range inventory {
		id := strings.TrimSpace(it.GetSandboxId())
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, sandboxpkg.InventoryItem{SandboxID: id, State: it.GetState()})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	workerRepo := s.workerRepo.WithTx(tx)
	if _, err := workerRepo.Update(ctx, w); err != nil {
		return err
	}
	first, err := workerRepo.ClaimInventory(ctx, w.ID, w.FencingToken)
	if err != nil {
		return err
	}
	orphans, err := s.sandboxRepo.WithTx(tx).ReconcileInventory(ctx, w.ID, first, items)
	if err != nil {
		return err
	}

	if len(orphans) > 0 {
		if s.riverClient == nil {
			log.Printf("heartbeat: worker=%s reports %d orphan sandbox(es) %v; no river client, not removing", w.ID, len(orphans), orphans)
		} else {
			params := make([]river.InsertManyParams, 0, len(orphans))
			for _, id := range orphans {
				params = append(params, river.InsertManyParams{Args: RemoveOrphanArgs{WorkerID: w.ID, SandboxID: id}})
			}
			if _, err := s.riverClient.InsertManyTx(ctx, tx, params); err != nil {
				return err
			}
		}
	}

	return tx.Commit(ctx)
}

func workerFromHeartbeat(req *orchestratorv1.WorkerHeartbeat) workerpkg.Worker {
	workerStatus := workerpkg.WorkerStatus(req.GetStatus())
	switch workerStatus {
	case workerpkg.WorkerStatusActive, workerpkg.WorkerStatusInactive:
	default:
		workerStatus = workerpkg.WorkerStatusActive
	}
	return workerpkg.NewWorkerFromHeartbeat(workerpkg.HeartbeatParams{
		ID:           strings.TrimSpace(req.GetWorkerId()),
		Status:       workerStatus,
		Address:      req.GetAddress(),
		Port:         int(req.GetPort()),
		Capacity:     int(req.GetCapacity()),
		CpuBudget:    int(req.GetCpuBudget()),
		MemBudget:    int(req.GetMemBudget()),
		CpuUsage:     int(req.GetCpuUsage()),
		MemUsage:     int(req.GetMemUsage()),
		FencingToken: req.GetFencingToken(),
	})
}

// applyStatusFromState applies a guarded status update based on the reported
// Firecracker instance state. Updates are only applied when the sandbox is in
// a state the event handler owns. rowsAffected=0 means the guard rejected the
// update (sandbox in a state the job owns).
func (s *EventGRPCServer) applyStatusFromState(ctx context.Context, sandboxID, workerID string, token int64, state string) {
	target, allowedFrom, ok := statusTransitionForState(state)
	if !ok {
		return
	}

	n, err := s.sandboxRepo.UpdateStatusFenced(ctx, sandboxID, workerID, token, target, allowedFrom...)
	if err != nil {
		log.Printf("update sandbox status failed: sandbox=%s state=%s err=%v", sandboxID, state, err)
		return
	}
	if n == 0 {
		log.Printf("ignored sandbox event: sandbox=%s state=%s — current status not in allowed set, or sandbox not hosted by this worker incarnation", sandboxID, state)
	}
}

// statusTransitionForState returns the target status and the set of allowed
// source statuses for a reported firecracker-go-sdk InstanceInfo.State value
// ("Not started" | "Running" | "Paused"). The create-sandbox job owns all
// transitions before running; events only advance starting→running.
//
// Unlike Docker's die/stop/kill/oom, InstanceInfo has no "crashed"/"exited"
// state to drive a starting|running→failed transition — that signal doesn't
// exist here anymore. Detecting a dead sandbox needs a different mechanism
// (e.g. missed heartbeats), not this event stream.
func statusTransitionForState(state string) (sandboxpkg.Status, []sandboxpkg.Status, bool) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return sandboxpkg.StatusRunning, []sandboxpkg.Status{
			sandboxpkg.StatusStarting,
			sandboxpkg.StatusRunning,
		}, true
	default:
		return "", nil, false
	}
}

func ServeEventGRPC(address string, srv orchestratorv1.OrchestratorServiceServer, opts ...grpc.ServerOption) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(opts...)
	orchestratorv1.RegisterOrchestratorServiceServer(grpcServer, srv)
	return grpcServer.Serve(listener)
}

type EventClient struct {
	conn   *grpc.ClientConn
	client orchestratorv1.OrchestratorServiceClient
}

func NewEventClient(ctx context.Context, address string) (*EventClient, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &EventClient{conn: conn, client: orchestratorv1.NewOrchestratorServiceClient(conn)}, nil
}

func (c *EventClient) Client() orchestratorv1.OrchestratorServiceClient {
	return c.client
}

func (c *EventClient) Close() error {
	return c.conn.Close()
}
