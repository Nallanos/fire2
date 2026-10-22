//go:build integration

package divergence

import (
	"net/http"
	"testing"
	"time"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
)

// E1: the worker's only slot is held by a VM that vanished without an event.
// The database cannot know, so it refuses new sandboxes; once the liveness
// reaper has failed the ghost and cleared its worker link, the slot is free
// again and a creation goes through.
func TestE1_VanishedVMFreesItsSlotAfterReconciliation(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1", WithCapacity(1))
	ghost := c.Create()

	w.VMs.Vanish(ghost.ID)

	if _, code := c.TryCreate(0); code == http.StatusCreated {
		t.Logf("E1: a creation went through before the reconciliation")
	}

	start := time.Now()
	var fresh sandboxpkg.Sandbox
	if !eventually(t, settle, func() bool {
		sbx, code := c.TryCreate(0)
		fresh = sbx
		return code == http.StatusCreated
	}) {
		t.Fatalf("the slot of a vanished VM was never freed\n%s", c.dump())
	}
	t.Logf("E1: a creation went through %s after the first attempt", time.Since(start).Round(10*time.Millisecond))

	if got := c.Sandbox(ghost.ID); got.Status != sandboxpkg.StatusFailed || got.WorkerID != nil {
		t.Errorf("the vanished sandbox is %s on %s, want failed with no worker", got.Status, workerOf(got))
	}
	if fresh.WorkerID == nil || *fresh.WorkerID != "w1" {
		t.Errorf("the new sandbox is on %v, want w1", fresh.WorkerID)
	}
	assertConverged(t, c, settle)
}

// E2: the worker's capacity drops below the number of sandboxes it hosts. New
// creations are refused, and the sandboxes already there are left alone.
func TestE2_CapacityBelowWhatIsHosted(t *testing.T) {
	c := newCluster(t)
	w := c.AddWorker("w1", WithCapacity(3))
	hosted := []string{c.Create().ID, c.Create().ID, c.Create().ID}

	w.SetCapacity(1)
	if !eventually(t, settle, func() bool { return c.Worker("w1").Capacity == 1 }) {
		t.Fatalf("the orchestrator never saw the capacity drop\n%s", c.dump())
	}

	if sbx, code := c.TryCreate(0); code == http.StatusCreated {
		t.Errorf("a creation went through on a worker over its capacity: %s on %v", short(sbx.ID), sbx.WorkerID)
	}

	assertConverged(t, c, settle)
	for _, id := range hosted {
		if got := c.Sandbox(id).Status; got != sandboxpkg.StatusRunning {
			t.Errorf("sandbox %s is %s, want running: lowering the capacity must not touch it", short(id), got)
		}
	}
	if removed := w.VMs.Removed(); len(removed) != 0 {
		t.Errorf("VMs were removed: %v", removed)
	}
	assertStable(t, c, time.Second)
}
