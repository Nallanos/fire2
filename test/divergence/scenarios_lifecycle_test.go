//go:build integration

package divergence

import (
	"context"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

// C1: the worker boots the VM but the response is lost. The create job retries,
// and the idempotent create must leave exactly one VM.
func TestC1_LostCreateResponseBootsOneVM(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	w.LoseNextCreateResponse()

	sbx := c.Create()

	if got := w.VMs.Boots(sbx.ID); got != 1 {
		t.Errorf("VM booted %d times, want once", got)
	}
	if hosted := w.VMs.Hosted(); len(hosted) != 1 {
		t.Errorf("worker hosts %d VMs, want 1: %v", len(hosted), hosted)
	}
	assertConverged(t, c, settle)
}

// C2: the worker boots the VM on every attempt but the orchestrator never gets
// an answer, so the create job runs out of attempts. The sandbox goes to
// cleanup_pending, the worker is asked to remove the VM, and it ends failed.
//
// Liveness is set out of reach: retries slower than its delay (a loaded machine
// is enough) would make it fail the sandbox between two attempts, which is what
// C5 checks on its own.
func TestC2_CreateJobExhaustsItsAttemptsAfterBooting(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(time.Hour))
	w := c.AddWorker("w1")
	w.LoseCreateResponses(1000)

	if _, code := c.TryCreate(0); code == http.StatusCreated {
		t.Fatalf("create answered 201 although no response ever reached the orchestrator")
	}
	rows := c.Sandboxes()
	if len(rows) != 1 {
		t.Fatalf("want one sandbox row, got %d\n%s", len(rows), c.dump())
	}
	id := rows[0].ID

	got := c.WaitStatus(id, settle, sandboxpkg.StatusFailed)
	if got.WorkerID != nil {
		t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
	}
	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(id) }) {
		t.Fatalf("the VM booted for the failed sandbox is still on the worker\n%s", c.dump())
	}
	if w.VMs.RemoveCalls(id) == 0 {
		t.Errorf("the worker never received RemoveSandbox")
	}
	if boots := w.VMs.Boots(id); boots != 1 {
		t.Errorf("VM booted %d times, want once: create must be idempotent across attempts", boots)
	}
	assertConverged(t, c, settle)
}

// C3: the worker refuses to create (ResourceExhausted, it is out of resources
// the orchestrator does not know about). The sandbox ends failed and nothing is
// left on the worker. Liveness is set out of reach, as in C2.
func TestC3_WorkerRefusesTheCreate(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(time.Hour))
	w := c.AddWorker("w1")
	w.VMs.SetFull(true)

	if _, code := c.TryCreate(0); code == http.StatusCreated {
		t.Fatalf("create answered 201 although the worker refused it")
	}
	rows := c.Sandboxes()
	if len(rows) != 1 {
		t.Fatalf("want one sandbox row, got %d\n%s", len(rows), c.dump())
	}
	got := c.WaitStatus(rows[0].ID, settle, sandboxpkg.StatusFailed)
	if got.WorkerID != nil {
		t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
	}
	if hosted := w.VMs.Hosted(); len(hosted) != 0 {
		t.Errorf("the worker hosts %v although it refused the create", hosted)
	}
	assertConverged(t, c, settle)
}

// C5: liveness fails a sandbox while its create job is still retrying. The
// worker refuses every create and is slow to answer, so the sandbox sits
// `assigned`, unlisted, for longer than the liveness delay; the reaper claims it
// and the cleanup fails it between two attempts. The job must then end as
// cancelled, not as completed: a completed job made the API answer 201 for a
// failed sandbox.
func TestC5_SandboxFailedUnderItsCreateJobIsNotReportedCreated(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(300*time.Millisecond))
	w := c.AddWorker("w1")
	w.VMs.SetFull(true)
	w.SlowCreates(time.Second)

	sbx, code := c.TryCreate(0)
	if code == http.StatusCreated {
		t.Fatalf("create answered 201 for a sandbox that is %s: the job completed although its sandbox ended without coming up", sbx.Status)
	}
	if code != http.StatusBadGateway {
		t.Errorf("create answered HTTP %d, want 502", code)
	}

	rows := c.Sandboxes()
	if len(rows) != 1 {
		t.Fatalf("want one sandbox row, got %d\n%s", len(rows), c.dump())
	}
	c.WaitStatus(rows[0].ID, settle, sandboxpkg.StatusFailed)
	assertConverged(t, c, settle)
}

// C4: a sandbox is deleted while its worker is unreachable. The cleanup gives up
// and drops the worker link; when the worker is back, its VM is removed as an
// orphan.
func TestC4_DeleteWhileTheWorkerIsUnreachable(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	sbx := c.Create()

	w.Partition(true)
	if code := c.DeleteSandbox(sbx.ID); code != http.StatusAccepted {
		t.Fatalf("delete: HTTP %d, want 202", code)
	}
	got := c.WaitStatus(sbx.ID, settle, sandboxpkg.StatusStopped, sandboxpkg.StatusFailed)
	t.Logf("C4: the deleted sandbox ended %s (the removal could not be confirmed)", got.Status)
	if got.WorkerID != nil {
		t.Errorf("sandbox is %s but still linked to worker %s", got.Status, *got.WorkerID)
	}
	if !w.VMs.Hosts(sbx.ID) {
		t.Fatalf("the VM is gone although the worker was unreachable\n%s", c.dump())
	}

	w.Partition(false)
	assertConverged(t, c, settle)
	if w.VMs.RemoveCalls(sbx.ID) == 0 {
		t.Errorf("the orphan VM was never removed")
	}
}

// D1: a VM dies and its event is lost. The inventory alone must fail the
// sandbox and clean it up. Liveness is set out of reach, so only the inventory
// can have done it.
func TestD1_LostDeathEventIsCaughtByTheInventory(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(time.Hour))
	w := c.AddWorker("w1")
	dead, alive := c.Create(), c.Create()

	w.VMs.Die(dead.ID, false)

	got := c.WaitStatus(dead.ID, settle, sandboxpkg.StatusFailed)
	if got.WorkerID != nil {
		t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
	}
	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(dead.ID) }) {
		t.Errorf("the dead VM was never removed from the worker\n%s", c.dump())
	}
	if got := c.Sandbox(alive.ID).Status; got != sandboxpkg.StatusRunning {
		t.Errorf("the healthy sandbox is %s, want running", got)
	}
	assertConverged(t, c, settle)
}

// D2: events duplicated, out of order, or about sandboxes that are not the
// worker's to report. The guards hold: no sandbox changes state because of
// them, and the inventory converges.
func TestD2_DuplicatedAndMisorderedEvents(t *testing.T) {
	c := newCluster(t)
	c.AddWorker("w1")
	running := c.Create()
	deleted := c.Create()
	if code := c.DeleteSandbox(deleted.ID); code != http.StatusAccepted {
		t.Fatalf("delete: HTTP %d, want 202", code)
	}
	c.WaitStatus(deleted.ID, settle, sandboxpkg.StatusStopped, sandboxpkg.StatusFailed)
	assertConverged(t, c, settle)

	orch := c.Orchestrator()
	token, _, _ := c.tokens("w1")
	now := time.Now()
	send := func(label, eventID, sandboxID string, state orchestratorv1.SandboxState, at time.Time, tok int64) codes.Code {
		t.Helper()
		_, err := orch.IngestSandboxEvent(context.Background(), &orchestratorv1.SandboxEvent{
			Id:           eventID,
			SandboxId:    sandboxID,
			WorkerId:     "w1",
			State:        state,
			OccurredAt:   timestamppb.New(at),
			FencingToken: tok,
		})
		code := status.Code(err)
		t.Logf("D2: %s -> %s", label, code)
		return code
	}
	const (
		runningState = orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING
		deadState    = orchestratorv1.SandboxState_SANDBOX_STATE_DEAD
		startState   = orchestratorv1.SandboxState_SANDBOX_STATE_STARTING
	)

	// Duplicates: the same state again, then the very same event again.
	send("RUNNING (first)", "d2-dup", running.ID, runningState, now, token)
	send("RUNNING (same state, new event)", "d2-dup-2", running.ID, runningState, now, token)
	send("RUNNING (same event ID again)", "d2-dup", running.ID, runningState, now, token)
	// Out of order: the death arrives, then an older RUNNING.
	send("DEAD", "d2-dead", running.ID, deadState, now.Add(time.Second), token)
	send("RUNNING (older than the DEAD)", "d2-late", running.ID, runningState, now.Add(-time.Minute), token)
	send("STARTING (after RUNNING)", "d2-back", running.ID, startState, now.Add(2*time.Second), token)
	// Not the worker's to report: a sandbox it no longer hosts, one that does not
	// exist, and an event carrying a token that is not w1's current one.
	send("RUNNING for a deleted sandbox", "d2-zombie", deleted.ID, runningState, now, token)
	send("RUNNING for an unknown sandbox", "d2-unknown", "00000000-0000-0000-0000-000000000000", runningState, now, token)
	if code := send("RUNNING with another incarnation's token", "d2-stale", running.ID, runningState, now, token+1); code != codes.FailedPrecondition {
		t.Errorf("an event with the wrong token got %s, want FailedPrecondition", code)
	}

	if got := c.Sandbox(running.ID).Status; got != sandboxpkg.StatusRunning {
		t.Errorf("the live sandbox is %s after the events, want running", got)
	}
	if got := c.Sandbox(deleted.ID); got.Status == sandboxpkg.StatusRunning || got.WorkerID != nil {
		t.Errorf("a RUNNING event revived the deleted sandbox: %s on %s", got.Status, workerOf(got))
	}
	assertConverged(t, c, settle)
}

// D3: sandboxes stuck in pending or scheduling with no job behind them (the
// create job was discarded: its last attempt could not reach the database to run
// the cleanup). Liveness only looks at sandboxes with a worker, so the TTL was
// the only thing that could end them, and the default TTL is an hour. The
// creation timeout fails them long before.
func TestD3_StalledCreationIsFailedByTheCreateTimeout(t *testing.T) {
	const createTimeout = 2 * time.Second
	c := newCluster(t, WithCreateTimeout(createTimeout))
	c.AddWorker("w1")
	stuck := []sandboxpkg.Sandbox{
		c.SeedSandbox(sandboxpkg.StatusScheduling, time.Hour),
		c.SeedSandbox(sandboxpkg.StatusPending, time.Hour),
	}

	start := time.Now()
	for _, sbx := range stuck {
		got := c.WaitStatus(sbx.ID, settle, sandboxpkg.StatusFailed)
		if got.WorkerID != nil {
			t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
		}
	}
	elapsed := time.Since(start)
	t.Logf("D3: stalled sandboxes failed %s after they were created (create timeout %s, TTL 1h)",
		elapsed.Round(10*time.Millisecond), createTimeout)

	if elapsed < createTimeout-500*time.Millisecond {
		t.Errorf("failed after %s, before the %s create timeout: a creation that may still be running was reaped", elapsed, createTimeout)
	}
	assertConverged(t, c, settle)
}
