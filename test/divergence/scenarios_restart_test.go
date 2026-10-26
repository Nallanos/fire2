//go:build integration

package divergence

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// B1: VMs die while the worker process is down. The new process either lists
// one as DEAD (its shim survived and knows how it ended) or does not list it at
// all (nothing to take back, the worker reaped it on startup). The first
// inventory of the new incarnation must fail both right away. The liveness
// delay is set out of reach, so only that inventory can have done it.
func TestB1_VMsDieWhileTheWorkerIsDown(t *testing.T) {
	c := newCluster(t, WithLivenessTimeout(time.Hour))
	w := c.AddWorker("w1")
	dead, lost, alive := c.Create(), c.Create(), c.Create()

	w.Crash()
	w.VMs.Die(dead.ID, false)
	w.VMs.Vanish(lost.ID)
	w.Restart()
	restarted := time.Now()

	for name, id := range map[string]string{"listed DEAD": dead.ID, "not listed": lost.ID} {
		got := c.WaitStatus(id, settle, sandboxpkg.StatusFailed)
		t.Logf("B1: %s: failed %s after the restart, from the first inventory", name, time.Since(restarted).Round(10*time.Millisecond))
		if got.WorkerID != nil {
			t.Errorf("sandbox (%s) is failed but still linked to worker %s", name, *got.WorkerID)
		}
	}
	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(dead.ID) }) {
		t.Errorf("the dead VM was never removed from the worker\n%s", c.dump())
	}
	if got := c.Sandbox(alive.ID).Status; got != sandboxpkg.StatusRunning {
		t.Errorf("the healthy sandbox is %s, want running", got)
	}
	assertConverged(t, c, settle)
}

// B2: the worker hosts a VM the database knows nothing about. The orchestrator
// must have the worker remove it.
func TestB2_UnknownVMIsRemoved(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")

	orphan := uuid.NewString()
	w.VMs.Adopt(orphan)

	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(orphan) }) {
		t.Fatalf("the unknown VM is still on the worker after %s\n%s", settle, c.dump())
	}
	if w.VMs.RemoveCalls(orphan) == 0 {
		t.Errorf("the VM is gone but the worker never received RemoveSandbox")
	}
	assertConverged(t, c, settle)
}

// B3: a second process takes over a worker ID while the first keeps running.
// The first one holds a stale fencing token: it must change neither the worker
// row nor the sandboxes, whatever it reports.
func TestB3_StaleProcessCannotModifyAnything(t *testing.T) {
	c := newCluster(t)
	stale := c.AddWorker("w1", WithCapacity(4))
	sbx := c.Create()

	// The new process starts over the VM, as a restarted worker takes it back.
	vms := NewFakeVMs()
	vms.Adopt(sbx.ID)
	current := c.AddWorker("w1", WithVMs(vms), WithCapacity(6))

	// The stale process now reports its VM dead, in its inventory and in an event.
	stale.VMs.Die(sbx.ID, true)
	if !eventually(t, 10*time.Second, func() bool { return stale.Heartbeats(codes.FailedPrecondition) >= 5 }) {
		t.Fatalf("the stale process's heartbeats were not refused\n%s", c.dump())
	}
	assertStable(t, c, time.Second)

	row := c.Worker("w1")
	if row.Port != current.Port() || row.Capacity != 6 {
		t.Errorf("worker row is port=%d capacity=%d, want the current process's port=%d capacity=6: the stale process wrote it",
			row.Port, row.Capacity, current.Port())
	}
	if got := c.Sandbox(sbx.ID).Status; got != sandboxpkg.StatusRunning {
		t.Errorf("sandbox is %s, want running: the stale process changed it", got)
	}
	var events int
	if err := c.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sandbox_events WHERE sandbox_id = $1 AND state = 'dead'`, sbx.ID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d event(s) of the stale process were stored", events)
	}
	if n := stale.VMs.RemoveCalls(sbx.ID); n != 0 {
		t.Errorf("the stale process was asked to remove the sandbox %d time(s): the orchestrator acted on its inventory", n)
	}
	assertConverged(t, c, settle)
}

// B4: the database loses track of a healthy worker's fencing token, as when it
// is restored from an older backup. The worker must find its way back: the
// orchestrator answers its heartbeat NotFound (the database is behind, this is
// not an older process), the worker registers again presenting the token it
// held, and its heartbeats are accepted again with its VMs untouched. B3 is the
// other side: an older process presents a token behind the database's and stays
// refused for good.
func TestB4_WorkerRecoversFromALostFencingToken(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	sbx := c.Create()

	if _, err := c.Pool.Exec(context.Background(),
		`UPDATE worker SET fencing_token = 0, inventory_token = 0 WHERE id = $1`, "w1"); err != nil {
		t.Fatalf("reset fencing token: %v", err)
	}

	const within = 30 * time.Second
	lost := time.Now()
	recovered := eventually(t, within, func() bool {
		token, inventoryToken, found := c.tokens("w1")
		return found && token > 0 && inventoryToken == token
	})
	if recovered {
		t.Logf("B4: w1 registered again and its inventory reconciled %s after the database lost its token", time.Since(lost).Round(10*time.Millisecond))
	}
	if !recovered {
		token, inventoryToken, _ := c.tokens("w1")
		gap(t, "B4",
			fmt.Sprintf("w1 de nouveau enregistré et ses heartbeats acceptés sous %s", within),
			fmt.Sprintf("jeton=%d inventory_token=%d, %d heartbeats refusés (FailedPrecondition), sandbox %s %s\n%s",
				token, inventoryToken, w.Heartbeats(codes.FailedPrecondition), short(sbx.ID), c.Sandbox(sbx.ID).Status, c.dump()))
	}
	assertConverged(t, c, settle)
	if got := c.Sandbox(sbx.ID).Status; got != sandboxpkg.StatusRunning {
		t.Errorf("sandbox %s is %s after the worker registered again, want running", short(sbx.ID), got)
	}
	if removed := w.VMs.Removed(); len(removed) != 0 {
		t.Errorf("healthy VMs were removed: %v", removed)
	}
}

// B5: heartbeats without an inventory (inventory_complete=false) reconcile
// nothing. Only liveness applies: a sandbox nobody lists ages out, an unknown
// VM is left alone until an inventory comes back.
func TestB5_HeartbeatsWithoutAnInventory(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1")
	sbx := c.Create()

	w.StripInventory(true)
	orphan := uuid.NewString()
	w.VMs.Adopt(orphan)

	time.Sleep(time.Second)
	if !w.VMs.Hosts(orphan) {
		t.Fatalf("a heartbeat without an inventory reconciled it: the unknown VM was removed")
	}

	got := c.WaitStatus(sbx.ID, settle, sandboxpkg.StatusFailed)
	if got.WorkerID != nil {
		t.Errorf("sandbox is failed but still linked to worker %s", *got.WorkerID)
	}
	if !eventually(t, settle, func() bool { return !w.VMs.Hosts(sbx.ID) }) {
		t.Errorf("the VM of the sandbox failed by liveness was never removed\n%s", c.dump())
	}
	if status := c.Worker("w1").Status; status != workerpkg.WorkerStatusActive {
		t.Errorf("w1 is %s although its heartbeats are accepted, want active", status)
	}
	if !w.VMs.Hosts(orphan) {
		t.Errorf("the unknown VM was removed although no inventory was reported")
	}

	w.StripInventory(false)
	assertConverged(t, c, settle)
}
