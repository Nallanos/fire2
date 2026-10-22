//go:build integration

package divergence

import (
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// A1: the worker process crashes and comes back a second later. Its VMs lived
// on under their shims, so nothing may be cleaned up.
func TestA1_WorkerCrashesAndComesBack(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	s1, s2 := c.Create(), c.Create()

	w.Crash()
	time.Sleep(time.Second)
	w.Restart()
	c.WaitSynced(w)

	assertConverged(t, c, settle)
	for _, id := range []string{s1.ID, s2.ID} {
		if got := c.Sandbox(id).Status; got != sandboxpkg.StatusRunning {
			t.Errorf("sandbox %s is %s after the worker came back, want running", short(id), got)
		}
	}
	if removed := w.VMs.Removed(); len(removed) != 0 {
		t.Errorf("healthy VMs were removed: %v", removed)
	}
	assertStable(t, c, time.Second)
}

// A2: the whole machine is lost, VMs included. The worker must be shown
// inactive, its sandboxes failed with the worker link dropped, and new
// sandboxes must go to the other worker.
func TestA2_MachineLost(t *testing.T) {
	c := newCluster(t)
	w1 := c.AddWorker("w1")
	s1, s2 := c.Create(), c.Create() // w1 is the only worker: both land there
	c.AddWorker("w2")

	killed := time.Now()
	w1.KillMachine()

	if !eventually(t, settle, func() bool { return c.Worker("w1").Status == workerpkg.WorkerStatusInactive }) {
		t.Fatalf("w1 still active %s after its machine was lost\n%s", settle, c.dump())
	}
	t.Logf("A2: w1 shown inactive %s after the machine was lost (HEARTBEAT_TIMEOUT %s, reaper every %s)",
		time.Since(killed).Round(10*time.Millisecond), heartbeatTimeout, reaperInterval)

	for _, id := range []string{s1.ID, s2.ID} {
		sbx := c.WaitStatus(id, settle, sandboxpkg.StatusFailed)
		if sbx.WorkerID != nil {
			t.Errorf("sandbox %s is failed but still linked to worker %s", short(id), *sbx.WorkerID)
		}
	}
	t.Logf("A2: both sandboxes failed %s after the machine was lost (liveness timeout %s)",
		time.Since(killed).Round(10*time.Millisecond), livenessTimeout)

	// A machine that is gone refuses the scheduler's GetWorkerInfo call, so it
	// is no candidate at all. The scheduler's own staleness check is 15s,
	// hard-coded in buildCandidates, whatever HEARTBEAT_TIMEOUT says: it only
	// matters for a worker that still answers but no longer heartbeats.
	for i := 0; i < 3; i++ {
		sbx := c.Create()
		if *sbx.WorkerID != "w2" {
			t.Fatalf("a new sandbox went to %s, want w2: the lost worker must not be scheduled", *sbx.WorkerID)
		}
	}
	assertConverged(t, c, settle)
}

// A3: the incident. A worker is replaced by an old binary that the
// orchestrator refuses (heartbeats without a fencing token) but whose gRPC
// server still answers. The orchestrator sees a silent worker: its sandbox must
// end failed and its VM removed. When a current worker comes back, everything
// is true again.
func TestA3_WorkerReplacedByARefusedOldBinary(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	sbx := c.Create()

	port := w.Port()
	w.Crash()
	legacy := c.AddLegacyWorker("w1", w.VMs, port)

	if !eventually(t, 10*time.Second, func() bool { return legacy.Heartbeats(codes.InvalidArgument) > 0 }) {
		t.Fatalf("the old worker's heartbeats were never refused\n%s", c.dump())
	}

	got := c.WaitStatus(sbx.ID, settle, sandboxpkg.StatusFailed)
	if got.WorkerID != nil {
		t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
	}
	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(sbx.ID) }) {
		t.Fatalf("the VM of a failed sandbox is still on the worker that answers\n%s", c.dump())
	}
	// The worker reaper and liveness both run on the same 2s delay, so w1 may
	// not be marked yet when the sandbox is already failed.
	if !eventually(t, settle, func() bool { return c.Worker("w1").Status == workerpkg.WorkerStatusInactive }) {
		t.Errorf("w1 is %s while its heartbeats are refused, want inactive", c.Worker("w1").Status)
	}

	legacy.Stop()
	w.Restart()
	c.WaitSynced(w)
	assertConverged(t, c, settle)
}

// A4: a sandbox's TTL expires while its worker is cut off from the
// orchestrator. The reaper claims it, the cleanup cannot reach the worker and
// gives up: the sandbox ends terminal with its worker link dropped, and when the
// network comes back the VM is removed as an orphan.
//
// Liveness is set out of reach so that the TTL is what ends the sandbox. The
// cleanup job may only end it as stopped if the removal is confirmed; with the
// worker unreachable it gives up after its attempts and the sandbox ends
// failed, which is the documented behavior (cleanup_sandbox_worker.go).
func TestA4_TTLExpiresDuringAPartition(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(time.Hour))
	w := c.AddWorker("w1")
	sbx, code := c.TryCreate(3 * time.Second)
	if code != http.StatusCreated {
		t.Fatalf("create sandbox: HTTP %d", code)
	}

	w.Partition(true)
	got := c.WaitStatus(sbx.ID, settle, sandboxpkg.StatusStopped, sandboxpkg.StatusFailed)
	t.Logf("A4: the expired sandbox ended %s (the removal could not be confirmed)", got.Status)
	if got.WorkerID != nil {
		t.Errorf("sandbox is %s but still linked to worker %s", got.Status, *got.WorkerID)
	}
	if !w.VMs.Hosts(sbx.ID) {
		t.Fatalf("the VM is gone although the worker was unreachable: someone reached it\n%s", c.dump())
	}

	w.Partition(false)
	assertConverged(t, c, settle)
	if w.VMs.RemoveCalls(sbx.ID) == 0 {
		t.Errorf("the orphan VM was never removed")
	}
}

// A5: a one-second partition, shorter than every delay. Nothing may move: no
// sandbox failed, no VM killed, no worker excluded.
func TestA5_ShortPartitionChangesNothing(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	s1, s2 := c.Create(), c.Create()

	w.Partition(true)
	assertStable(t, c, time.Second)
	w.Partition(false)
	assertStable(t, c, time.Second)

	assertConverged(t, c, settle)
	for _, id := range []string{s1.ID, s2.ID} {
		if got := c.Sandbox(id).Status; got != sandboxpkg.StatusRunning {
			t.Errorf("sandbox %s is %s, want running", short(id), got)
		}
	}
	if removed := w.VMs.Removed(); len(removed) != 0 {
		t.Errorf("healthy VMs were removed: %v", removed)
	}
}
