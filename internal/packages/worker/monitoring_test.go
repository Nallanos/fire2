package worker

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type heartbeatClientStub struct {
	mu                 sync.Mutex
	registrationToken  int64
	registrationDelay  time.Duration
	registrations      int
	registrationTokens []int64
	heartbeatTokens    []int64
	heartbeatSandboxes [][]string
	heartbeatComplete  []bool
}

func (c *heartbeatClientStub) IngestSandboxEvent(context.Context, *orchestratorv1.SandboxEvent, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (c *heartbeatClientStub) RegisterWorker(_ context.Context, req *orchestratorv1.WorkerHeartbeat, _ ...grpc.CallOption) (*orchestratorv1.WorkerRegistration, error) {
	c.mu.Lock()
	c.registrations++
	c.registrationTokens = append(c.registrationTokens, req.GetFencingToken())
	token := c.registrationToken
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
	c.heartbeatSandboxes = append(c.heartbeatSandboxes, req.GetSandboxIds())
	c.heartbeatComplete = append(c.heartbeatComplete, req.GetInventoryComplete())
	return &emptypb.Empty{}, nil
}

func TestUpdateWorkerRegistersOnceAndReportsFencingToken(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 42}
	w := NewWorkerService(client)
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

func TestUpdateWorkerSerializesConcurrentRegistration(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 17, registrationDelay: 20 * time.Millisecond}
	w := NewWorkerService(client)
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
	w := NewWorkerService(client)
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

// The heartbeat inventory is the key set of the worker's in-memory sandbox
// map: complete, with no probe of the VMs (the map holds no live clients here,
// so any probe would fail or panic).
func TestUpdateWorkerReportsTrackedSandboxIDs(t *testing.T) {
	client := &heartbeatClientStub{registrationToken: 7}
	w := NewWorkerService(client)
	w.SetWorkerIdentity("worker-inv", "127.0.0.1")
	w.SetWorkerBudget(2, 1024)

	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("UpdateWorker (empty): %v", err)
	}

	w.mu.Lock()
	w.runningSandboxes["sbx-a"] = nil
	w.runningSandboxes["sbx-b"] = nil
	w.mu.Unlock()
	if _, err := w.UpdateWorker(context.Background()); err != nil {
		t.Fatalf("UpdateWorker: %v", err)
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.heartbeatSandboxes) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", len(client.heartbeatSandboxes))
	}
	if !client.heartbeatComplete[0] || len(client.heartbeatSandboxes[0]) != 0 {
		t.Fatalf("empty worker must send a complete, empty inventory: complete=%v ids=%v",
			client.heartbeatComplete[0], client.heartbeatSandboxes[0])
	}
	got := append([]string(nil), client.heartbeatSandboxes[1]...)
	sort.Strings(got)
	if !client.heartbeatComplete[1] || strings.Join(got, ",") != "sbx-a,sbx-b" {
		t.Fatalf("inventory must list tracked sandbox IDs: complete=%v ids=%v", client.heartbeatComplete[1], got)
	}
}
