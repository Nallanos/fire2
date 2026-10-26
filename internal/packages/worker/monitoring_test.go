package worker

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type heartbeatClientStub struct {
	mu                 sync.Mutex
	registrationToken  int64
	registrationDelay  time.Duration
	registrations      int
	registrationTokens []int64
	heartbeatTokens    []int64
	heartbeatSandboxes [][]*orchestratorv1.SandboxInventoryEntry
	heartbeatComplete  []bool
	// heartbeatErrs are returned, in order, by the next heartbeats; once used up
	// they are accepted. The heartbeat is still recorded.
	heartbeatErrs []error
	// registrationTokenFor, if set, picks the token a registration returns from
	// the token presented in it.
	registrationTokenFor func(presented int64) int64
}

func (c *heartbeatClientStub) IngestSandboxEvent(context.Context, *orchestratorv1.SandboxEvent, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (c *heartbeatClientStub) RegisterWorker(_ context.Context, req *orchestratorv1.WorkerHeartbeat, _ ...grpc.CallOption) (*orchestratorv1.WorkerRegistration, error) {
	c.mu.Lock()
	c.registrations++
	c.registrationTokens = append(c.registrationTokens, req.GetFencingToken())
	token := c.registrationToken
	if c.registrationTokenFor != nil {
		token = c.registrationTokenFor(req.GetFencingToken())
	}
	delay := c.registrationDelay
	c.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	return &orchestratorv1.WorkerRegistration{FencingToken: token}, nil
}

func (c *heartbeatClientStub) ReportWorkerHeartbeat(_ context.Context, req *orchestratorv1.WorkerHeartbeat, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeatTokens = append(c.heartbeatTokens, req.GetFencingToken())
	c.heartbeatSandboxes = append(c.heartbeatSandboxes, req.GetSandboxes())
	c.heartbeatComplete = append(c.heartbeatComplete, req.GetInventoryComplete())
	if len(c.heartbeatErrs) > 0 {
		err := c.heartbeatErrs[0]
		c.heartbeatErrs = c.heartbeatErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	return &emptypb.Empty{}, nil
}

type sandboxManagerStub struct {
	mu        sync.Mutex
	inventory []*orchestratorv1.SandboxInventoryEntry
	capacity  int
}

func (s *sandboxManagerStub) Create(sandboxmgr.CreateInput) error { return nil }
func (s *sandboxManagerStub) Stop(string) error                   { return nil }
func (s *sandboxManagerStub) Remove(string) error                 { return nil }
func (s *sandboxManagerStub) Events() <-chan sandboxmgr.StateChange {
	return nil
}

func (s *sandboxManagerStub) Snapshot() []*orchestratorv1.SandboxInventoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*orchestratorv1.SandboxInventoryEntry(nil), s.inventory...)
}

func (s *sandboxManagerStub) SetCapacity(capacity int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capacity = capacity
}

func TestUpdateWorkerRegistersOnceAndReportsFencingToken(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 42}
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-1", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	for range 2 {
		id, err := w.UpdateWorker(context.Background())
		if err != nil || id != "worker-1" {
			t.Fatalf("UpdateWorker: id=%q err=%v", id, err)
		}
	}

	if client.registrations != 1 || len(client.registrationTokens) != 1 || client.registrationTokens[0] != 0 {
		t.Fatalf("registration calls/tokens: %d, %v", client.registrations, client.registrationTokens)
	}
	if len(client.heartbeatTokens) != 2 || client.heartbeatTokens[0] != 42 || client.heartbeatTokens[1] != 42 {
		t.Fatalf("heartbeat tokens: %v", client.heartbeatTokens)
	}
	info, err := w.GetWorkerInfo(context.Background())
	if err != nil || info.FencingToken != 42 {
		t.Fatalf("worker fencing token: %d, err=%v", info.FencingToken, err)
	}
}

// The orchestrator says NotFound when its database lost the worker's token (row
// deleted, restored from a backup): this process is not an old one, so it
// registers again on the next heartbeat, presenting the token it held so the new
// one is above it, and carries on with that token.
func TestUpdateWorkerRegistersAgainWhenTheOrchestratorForgotIt(t *testing.T) {
	client := &heartbeatClientStub{
		registrationTokenFor: func(presented int64) int64 { return presented + 1 },
		heartbeatErrs:        []error{nil, status.Error(codes.NotFound, "worker unknown, register again")},
	}
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-forgotten", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	if _, err := w.UpdateWorker(context.Background()); status.Code(err) != codes.NotFound {
		t.Fatalf("heartbeat the orchestrator does not know: want NotFound, got %v", err)
	}
	info, _ := w.GetWorkerInfo(context.Background())
	if info.FencingToken != 1 {
		t.Fatalf("the process must keep the token it held until it registers again, got %d", info.FencingToken)
	}

	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("heartbeat after being told to register again: %v", err)
	}
	if want := []int64{0, 1}; !slices.Equal(client.registrationTokens, want) {
		t.Fatalf("tokens presented at registration: got %v, want %v (the second one is the token it held)", client.registrationTokens, want)
	}
	if want := []int64{1, 1, 2}; !slices.Equal(client.heartbeatTokens, want) {
		t.Fatalf("heartbeat tokens: got %v, want %v", client.heartbeatTokens, want)
	}
	if info, _ := w.GetWorkerInfo(context.Background()); info.FencingToken != 2 {
		t.Fatalf("token after registering again: %d, want 2", info.FencingToken)
	}

	// Registered again: the next heartbeats do not register once more.
	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if len(client.registrationTokens) != 2 {
		t.Fatalf("registrations: %v, want 2", client.registrationTokens)
	}
}

// FailedPrecondition is an older process, refused for good: registering again
// would take the worker back from the process that replaced it.
func TestUpdateWorkerDoesNotRegisterAgainWhenFenced(t *testing.T) {
	client := &heartbeatClientStub{
		registrationToken: 3,
		heartbeatErrs:     []error{status.Error(codes.FailedPrecondition, "worker fencing token is stale")},
	}
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-stale", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	if _, err := w.UpdateWorker(context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	for range 3 {
		_, _ = w.UpdateWorker(context.Background())
	}
	if client.registrations != 1 {
		t.Fatalf("registrations: want 1 (startup only), got %d", client.registrations)
	}
}

func TestUpdateWorkerSerializesConcurrentRegistration(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 17, registrationDelay: 20 * time.Millisecond}
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-2", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.UpdateWorker(context.Background())
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("UpdateWorker: %v", err)
		}
	}
	if client.registrations != 1 {
		t.Fatalf("registrations: want 1, got %d", client.registrations)
	}
	if len(client.heartbeatTokens) != n {
		t.Fatalf("heartbeats: want %d, got %d", n, len(client.heartbeatTokens))
	}
	for _, token := range client.heartbeatTokens {
		if token != 17 {
			t.Fatalf("heartbeat token: want 17, got %d", token)
		}
	}
}

func TestUpdateWorkerRejectsInvalidRegistrationToken(t *testing.T) {
	client := &heartbeatClientStub{}
	w := NewWorkerService(client, &sandboxManagerStub{})
	w.SetWorkerIdentity("worker-3", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	_, err := w.UpdateWorker(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid fencing token") {
		t.Fatalf("want invalid fencing token error, got %v", err)
	}
	if len(client.heartbeatTokens) != 0 {
		t.Fatalf("unexpected heartbeat without valid token: %v", client.heartbeatTokens)
	}
	info, infoErr := w.GetWorkerInfo(context.Background())
	if infoErr != nil || info.FencingToken != 0 {
		t.Fatalf("worker token after failed registration: %d, err=%v", info.FencingToken, infoErr)
	}
}

// The heartbeat inventory is the sandbox manager's snapshot: every tracked
// sandbox with its last known state, complete, with no probe of the VMs. The
// heartbeat also hands the computed capacity to the manager.
func TestUpdateWorkerReportsSandboxInventory(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 7}
	sandboxes := &sandboxManagerStub{}
	w := NewWorkerService(client, sandboxes)
	w.SetWorkerIdentity("worker-inv", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("UpdateWorker (empty): %v", err)
	}

	sandboxes.mu.Lock()
	sandboxes.inventory = []*orchestratorv1.SandboxInventoryEntry{
		{SandboxId: "sbx-a", State: sandboxmgr.StateRunning},
		{SandboxId: "sbx-b", State: sandboxmgr.StateDead},
	}
	sandboxes.mu.Unlock()
	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.heartbeatSandboxes) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", len(client.heartbeatSandboxes))
	}
	if !client.heartbeatComplete[0] || len(client.heartbeatSandboxes[0]) != 0 {
		t.Fatalf("empty worker must send a complete, empty inventory: complete=%v entries=%v",
			client.heartbeatComplete[0], client.heartbeatSandboxes[0])
	}
	var got []string
	for _, e := range client.heartbeatSandboxes[1] {
		got = append(got, e.GetSandboxId()+"="+e.GetState().String())
	}
	sort.Strings(got)
	want := "sbx-a=SANDBOX_STATE_RUNNING,sbx-b=SANDBOX_STATE_DEAD"
	if !client.heartbeatComplete[1] || strings.Join(got, ",") != want {
		t.Fatalf("inventory = %v (complete=%v), want %s", got, client.heartbeatComplete[1], want)
	}

	sandboxes.mu.Lock()
	defer sandboxes.mu.Unlock()
	if sandboxes.capacity != 2 { // (1024-128)/256 = 3, cpu 2/1 = 2
		t.Fatalf("manager capacity = %d, want 2", sandboxes.capacity)
	}
}
