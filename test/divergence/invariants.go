//go:build integration

package divergence

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	sandboxpkg "github/nallanos/fire2/internal/packages/sandbox"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

const pollEvery = 25 * time.Millisecond

// eventually polls cond until it holds or within has elapsed, and reports
// whether it held. The caller words the failure, so it can say what it expected
// and what it saw.
func eventually(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pollEvery)
	}
}

// gap fails a scenario that found a hole in the system: the mechanism meant to
// converge did not. The message states what was expected, what was seen, and
// where the hole is written up.
func gap(t *testing.T, id, expected, observed string) {
	t.Helper()
	t.Fatalf("TROU %s : attendu %s, observé %s. Voir docs/state-divergence.md#%s",
		id, expected, observed, strings.ToLower(id))
}

// divergences lists every way the database and the machines disagree.
//
// The invariant: a sandbox the database says assigned|starting|running on
// worker W is a VM W hosts, alive, and every VM W hosts is such a sandbox.
// A sandbox still moving (pending, scheduling, cleanup_pending) is not settled
// either. And a worker that is up and reachable must not be shown as inactive.
func (c *Cluster) divergences() []string {
	ctx := context.Background()
	var out []string

	rows, err := c.sandboxes.List(ctx)
	if err != nil {
		return []string{fmt.Sprintf("cannot read sandboxes: %v", err)}
	}
	workers, err := c.workers.List(ctx)
	if err != nil {
		return []string{fmt.Sprintf("cannot read workers: %v", err)}
	}

	byID := make(map[string]sandboxpkg.Sandbox, len(rows))
	hosted := make(map[string]map[string]sandboxmgr.State)
	sims := c.registered()
	for _, w := range sims {
		hosted[w.ID] = w.VMs.states()
	}

	for _, s := range rows {
		byID[s.ID] = s
		switch s.Status {
		case sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning:
			if s.WorkerID == nil {
				out = append(out, fmt.Sprintf("sandbox %s is %s but has no worker", short(s.ID), s.Status))
				continue
			}
			st, ok := hosted[*s.WorkerID][s.ID]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("database: sandbox %s %s on worker %s, but that worker does not host its VM", short(s.ID), s.Status, *s.WorkerID))
			case st == sandboxmgr.StateDead:
				out = append(out, fmt.Sprintf("database: sandbox %s %s on worker %s, but its VM is dead", short(s.ID), s.Status, *s.WorkerID))
			}
		case sandboxpkg.StatusPending, sandboxpkg.StatusScheduling, sandboxpkg.StatusCleanupPending:
			out = append(out, fmt.Sprintf("sandbox %s is still %s: not settled", short(s.ID), s.Status))
		}
	}

	for _, w := range sims {
		for id := range hosted[w.ID] {
			s, ok := byID[id]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("worker %s hosts VM %s, unknown to the database", w.ID, short(id)))
			case !isActive(s.Status) || s.WorkerID == nil || *s.WorkerID != w.ID:
				out = append(out, fmt.Sprintf("worker %s hosts VM %s, but the database says %s on %s", w.ID, short(id), s.Status, workerOf(s)))
			}
		}
	}

	rowByID := make(map[string]workerpkg.Worker, len(workers))
	for _, w := range workers {
		rowByID[w.ID] = w
	}
	for _, w := range sims {
		row, ok := rowByID[w.ID]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("worker %s is up but has no row", w.ID))
		case w.Running() && !w.partitioned.Load() && row.Status != workerpkg.WorkerStatusActive:
			out = append(out, fmt.Sprintf("worker %s is up and reachable, but the database says %s", w.ID, row.Status))
		case !w.Running() && row.Status == workerpkg.WorkerStatusActive:
			out = append(out, fmt.Sprintf("worker %s is down, but the database says active", w.ID))
		}
	}

	sort.Strings(out)
	return out
}

func isActive(st sandboxpkg.Status) bool {
	switch st {
	case sandboxpkg.StatusAssigned, sandboxpkg.StatusStarting, sandboxpkg.StatusRunning:
		return true
	}
	return false
}

func workerOf(s sandboxpkg.Sandbox) string {
	if s.WorkerID == nil {
		return "no worker"
	}
	return *s.WorkerID
}

// assertConverged waits up to within for the database and the machines to
// agree, and fails with every remaining gap if they do not.
func assertConverged(t *testing.T, c *Cluster, within time.Duration) {
	t.Helper()
	var last []string
	ok := eventually(t, within, func() bool {
		last = c.divergences()
		return len(last) == 0
	})
	if !ok {
		t.Fatalf("the database still diverges from the machines after %s:\n  - %s\n%s",
			within, strings.Join(last, "\n  - "), c.dump())
	}
}

// fingerprint is everything a scenario that expects "nothing moves" watches:
// sandbox rows, the workers' status and tokens, and the VMs on the machines.
// The heartbeat time is left out, it moves on purpose.
func (c *Cluster) fingerprint() string {
	ctx := context.Background()
	var b strings.Builder

	rows, err := c.sandboxes.List(ctx)
	if err != nil {
		return fmt.Sprintf("cannot read sandboxes: %v", err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, s := range rows {
		fmt.Fprintf(&b, "sandbox %s %s %s\n", short(s.ID), s.Status, workerOf(s))
	}

	workers, err := c.workers.List(ctx)
	if err != nil {
		return fmt.Sprintf("cannot read workers: %v", err)
	}
	for _, w := range workers {
		token, inventoryToken, _ := c.tokens(w.ID)
		fmt.Fprintf(&b, "worker %s %s token=%d inventory_token=%d port=%d capacity=%d\n", w.ID, w.Status, token, inventoryToken, w.Port, w.Capacity)
	}

	for _, w := range c.registered() {
		for _, id := range w.VMs.Hosted() {
			st, _ := w.VMs.State(id)
			fmt.Fprintf(&b, "vm %s on %s %s\n", short(id), w.ID, st)
		}
	}
	return b.String()
}

// assertStable fails if anything moves during the given time.
func assertStable(t *testing.T, c *Cluster, during time.Duration) {
	t.Helper()
	before := c.fingerprint()
	deadline := time.Now().Add(during)
	for time.Now().Before(deadline) {
		time.Sleep(pollEvery)
		if now := c.fingerprint(); now != before {
			t.Fatalf("state moved within %s, it should have stayed still\nbefore:\n%s\nafter:\n%s", during, before, now)
		}
	}
}
