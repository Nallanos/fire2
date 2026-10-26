//go:build integration

package divergence

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// eventBuffer mirrors sandboxmgr: state changes wait here for the event
// reporter, and are dropped (never blocking) when it is full.
const eventBuffer = 256

var _ workerpkg.SandboxManager = (*FakeVMs)(nil)

// FakeVMs stands in for sandboxmgr.Manager: the VMs of one machine, in memory.
// It keeps the Manager's contract, which is what the worker and the
// orchestrator rely on:
//   - Create is idempotent, and a duplicate call does not count against capacity;
//   - capacity 0 means "not known yet, no limit", and DEAD VMs count against it;
//   - a VM that dies stays tracked, and listed as DEAD, until Remove;
//   - Remove is the only call that drops a VM.
//
// A FakeVMs outlives the worker process using it, the way VMs outlive the
// worker under their shims: a SimWorker restarted on the same FakeVMs "takes
// back" its VMs.
//
// The methods after the interface ones are test hooks: they act on the
// machine, not through the worker.
type FakeVMs struct {
	mu          sync.Mutex
	vms         map[string]sandboxmgr.State
	capacity    int
	full        bool
	events      chan sandboxmgr.StateChange
	boots       map[string]int
	removeCalls map[string]int
	removed     []string
}

func NewFakeVMs() *FakeVMs {
	return &FakeVMs{
		vms:         make(map[string]sandboxmgr.State),
		events:      make(chan sandboxmgr.StateChange, eventBuffer),
		boots:       make(map[string]int),
		removeCalls: make(map[string]int),
	}
}

// Create boots a VM. The Manager reports RUNNING as soon as the VM answers, so
// the event goes out right away.
func (f *FakeVMs) Create(in sandboxmgr.CreateInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vms[in.ID]; ok {
		return nil
	}
	if f.full || (f.capacity > 0 && len(f.vms) >= f.capacity) {
		return sandboxmgr.ErrAtCapacity
	}
	f.vms[in.ID] = sandboxmgr.StateRunning
	f.boots[in.ID]++
	f.emit(in.ID, sandboxmgr.StateRunning)
	return nil
}

// Stop kills the VM but keeps it tracked, DEAD, until Remove.
func (f *FakeVMs) Stop(sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.vms[sandboxID]
	if !ok {
		return fmt.Errorf("%w: %s", sandboxmgr.ErrSandboxNotFound, sandboxID)
	}
	if st != sandboxmgr.StateDead {
		f.vms[sandboxID] = sandboxmgr.StateDead
		f.emit(sandboxID, sandboxmgr.StateDead)
	}
	return nil
}

func (f *FakeVMs) Remove(sandboxID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls[sandboxID]++
	if _, ok := f.vms[sandboxID]; !ok {
		return fmt.Errorf("%w: %s", sandboxmgr.ErrSandboxNotFound, sandboxID)
	}
	delete(f.vms, sandboxID)
	f.removed = append(f.removed, sandboxID)
	return nil
}

func (f *FakeVMs) Snapshot() []*orchestratorv1.SandboxInventoryEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*orchestratorv1.SandboxInventoryEntry, 0, len(f.vms))
	for _, id := range f.sortedIDs() {
		out = append(out, &orchestratorv1.SandboxInventoryEntry{SandboxId: id, State: f.vms[id]})
	}
	return out
}

func (f *FakeVMs) SetCapacity(capacity int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity = capacity
}

func (f *FakeVMs) Events() <-chan sandboxmgr.StateChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.events
}

// emit never blocks. The caller holds f.mu.
func (f *FakeVMs) emit(sandboxID string, st sandboxmgr.State) {
	select {
	case f.events <- sandboxmgr.StateChange{SandboxID: sandboxID, State: st, At: time.Now().UTC()}:
	default:
	}
}

func (f *FakeVMs) sortedIDs() []string {
	ids := make([]string, 0, len(f.vms))
	for id := range f.vms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Die ends the VM's process: its state becomes DEAD and it stays tracked.
// emitEvent false loses the state-change event, as a full buffer or a failed
// send would; the inventory still reports DEAD.
func (f *FakeVMs) Die(sandboxID string, emitEvent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.vms[sandboxID]; !ok || st == sandboxmgr.StateDead {
		return
	}
	f.vms[sandboxID] = sandboxmgr.StateDead
	if emitEvent {
		f.emit(sandboxID, sandboxmgr.StateDead)
	}
}

// Adopt makes the machine host a running VM the orchestrator knows nothing
// about, without any event.
func (f *FakeVMs) Adopt(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[sandboxID] = sandboxmgr.StateRunning
}

// Vanish drops a VM without a trace: no event, no Remove call. The
// inventory just stops listing it.
func (f *FakeVMs) Vanish(sandboxID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vms, sandboxID)
}

// Wipe loses every VM, as the machine dying does.
func (f *FakeVMs) Wipe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.vms)
}

// SetFull makes Create answer ErrAtCapacity whatever the capacity, the way a
// worker that is out of resources the orchestrator does not know about does.
func (f *FakeVMs) SetFull(full bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.full = full
}

// newProcess is what a restarted worker process starts from: a fresh Manager
// that adopted the surviving VMs. The old process's pending events died with
// it, and its capacity is unknown until the first heartbeat.
func (f *FakeVMs) newProcess() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = make(chan sandboxmgr.StateChange, eventBuffer)
	f.capacity = 0
}

// states copies the tracked VMs with their state.
func (f *FakeVMs) states() map[string]sandboxmgr.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.vms)
}

// Hosted returns the IDs of the tracked VMs, DEAD ones included, sorted.
func (f *FakeVMs) Hosted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sortedIDs()
}

func (f *FakeVMs) Hosts(sandboxID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.vms[sandboxID]
	return ok
}

func (f *FakeVMs) State(sandboxID string) (sandboxmgr.State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.vms[sandboxID]
	return st, ok
}

// Boots is how many times a VM was actually booted (a duplicate Create does
// not boot again).
func (f *FakeVMs) Boots(sandboxID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boots[sandboxID]
}

// RemoveCalls is how many Remove calls reached the machine for the sandbox,
// whether or not it hosted it.
func (f *FakeVMs) RemoveCalls(sandboxID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removeCalls[sandboxID]
}

// Removed lists, in order, the VMs a Remove call dropped.
func (f *FakeVMs) Removed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}
