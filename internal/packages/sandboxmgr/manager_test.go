package sandboxmgr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeVM struct {
	mu        sync.Mutex
	state     State
	stateErr  error
	panicOn   bool
	stopped   bool
	cleaned   bool
	createErr error
	gate      chan struct{} // Create blocks until closed, if set
	exit      chan error    // Wait returns what is sent here
}

func (f *fakeVM) Create(context.Context) error {
	if f.gate != nil {
		<-f.gate
	}
	return f.createErr
}

func (f *fakeVM) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeVM) Wait(ctx context.Context) error {
	select {
	case err := <-f.exit:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeVM) State(context.Context) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOn {
		panic("boom")
	}
	return f.state, f.stateErr
}

func (f *fakeVM) Cleanup() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = true
	return nil
}

func (f *fakeVM) set(fn func(*fakeVM)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type harness struct {
	m *Manager

	mu       sync.Mutex
	vms      map[string]*fakeVM
	alive    map[string]*fakeVM // VMs a previous worker left running
	reaped   []string
	disk     []string
	reapGate chan struct{}
}

// newHarness builds a Manager whose next VMs come from template (copied per
// sandbox), polling every millisecond.
func newHarness(t *testing.T, template func(id string) *fakeVM) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{vms: make(map[string]*fakeVM), alive: make(map[string]*fakeVM)}
	newVM := func(in CreateInput) (vm, error) {
		v := template(in.ID)
		h.mu.Lock()
		h.vms[in.ID] = v
		h.mu.Unlock()
		return v, nil
	}
	reap := func(id string) error {
		h.mu.Lock()
		gate := h.reapGate
		h.reaped = append(h.reaped, id)
		h.mu.Unlock()
		if gate != nil {
			<-gate
		}
		return nil
	}
	listDisk := func() ([]string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]string(nil), h.disk...), nil
	}
	attach := func(id string) (vm, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if v, ok := h.alive[id]; ok {
			return v, nil
		}
		return nil, errors.New("no live shim")
	}
	h.m = newManager(ctx, newVM, attach, reap, listDisk)
	h.m.pollInterval = time.Millisecond
	return h
}

func runningVM(string) *fakeVM {
	return &fakeVM{state: StateRunning, exit: make(chan error, 1)}
}

func (h *harness) vm(id string) *fakeVM {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.vms[id]
}

func (h *harness) reapedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reaped...)
}

func nextEvent(t *testing.T, m *Manager) StateChange {
	t.Helper()
	select {
	case ev := <-m.Events():
		return ev
	case <-time.After(time.Second):
		t.Fatal("no state change emitted")
		return StateChange{}
	}
}

func noEvent(t *testing.T, m *Manager, wait time.Duration) {
	t.Helper()
	select {
	case ev := <-m.Events():
		t.Fatalf("unexpected state change: %+v", ev)
	case <-time.After(wait):
	}
}

func snapshotState(m *Manager, id string) (State, bool) {
	for _, e := range m.Snapshot() {
		if e.GetSandboxId() == id {
			return e.GetState(), true
		}
	}
	return 0, false
}

const sbx = "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01"

func TestCreateReportsRunningOnceOnly(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if ev := nextEvent(t, h.m); ev.SandboxID != sbx || ev.State != StateRunning {
		t.Fatalf("event = %+v, want %s RUNNING", ev, sbx)
	}
	// The state is polled every millisecond but doesn't change: diff-only.
	noEvent(t, h.m, 30*time.Millisecond)

	if st, ok := snapshotState(h.m, sbx); !ok || st != StateRunning {
		t.Fatalf("snapshot state = %v (tracked=%v), want RUNNING", st, ok)
	}
}

func TestQueryFailureIsUnknown(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	nextEvent(t, h.m) // RUNNING

	h.vm(sbx).set(func(f *fakeVM) { f.stateErr = errors.New("api timeout") })
	if ev := nextEvent(t, h.m); ev.State != StateUnknown {
		t.Fatalf("event = %+v, want UNKNOWN", ev)
	}
}

func TestProcessExitIsDeadAndStaysTracked(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	nextEvent(t, h.m) // RUNNING

	h.vm(sbx).exit <- errors.New("signal: killed")
	if ev := nextEvent(t, h.m); ev.State != StateDead {
		t.Fatalf("event = %+v, want DEAD", ev)
	}
	// DEAD is final: polling stops, nothing else is reported.
	noEvent(t, h.m, 30*time.Millisecond)

	if st, ok := snapshotState(h.m, sbx); !ok || st != StateDead {
		t.Fatalf("snapshot state = %v (tracked=%v), want DEAD and still tracked", st, ok)
	}

	if err := h.m.Remove(sbx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := snapshotState(h.m, sbx); ok {
		t.Fatal("removed sandbox still tracked")
	}
}

func TestPanickingServiceIsDeadAndRemoveReaps(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	nextEvent(t, h.m) // RUNNING

	h.vm(sbx).set(func(f *fakeVM) { f.panicOn = true })
	if ev := nextEvent(t, h.m); ev.State != StateDead {
		t.Fatalf("event = %+v, want DEAD", ev)
	}

	if err := h.m.Remove(sbx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := h.reapedIDs(); len(got) != 1 || got[0] != sbx {
		t.Fatalf("reaped = %v, want [%s]: no actor left to clean up", got, sbx)
	}
}

func TestLivenessMarksExitedServiceDead(t *testing.T) {
	h := newHarness(t, runningVM)

	// A Service whose goroutine exited without reporting (not a panic).
	done := make(chan struct{})
	close(done)
	h.m.mu.Lock()
	h.m.services[sbx] = &entry{svc: &Service{id: sbx, done: done}, state: StateRunning}
	h.m.mu.Unlock()

	h.m.checkLiveness()
	if ev := nextEvent(t, h.m); ev.SandboxID != sbx || ev.State != StateDead {
		t.Fatalf("event = %+v, want %s DEAD", ev, sbx)
	}
	if st, ok := snapshotState(h.m, sbx); !ok || st != StateDead {
		t.Fatalf("snapshot state = %v (tracked=%v), want DEAD and still tracked", st, ok)
	}
	h.m.checkLiveness()
	noEvent(t, h.m, 10*time.Millisecond)
}

func TestCreateIsIdempotent(t *testing.T) {
	h := newHarness(t, runningVM)
	h.m.SetCapacity(1)
	for range 2 {
		if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if n := len(h.m.Snapshot()); n != 1 {
		t.Fatalf("tracked = %d, want 1", n)
	}
}

func TestCapacityCountsDeadSandboxes(t *testing.T) {
	h := newHarness(t, runningVM)
	h.m.SetCapacity(1)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	nextEvent(t, h.m) // RUNNING
	h.vm(sbx).exit <- nil
	nextEvent(t, h.m) // DEAD

	err := h.m.Create(CreateInput{ID: "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c02"})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("Create = %v, want ErrAtCapacity: a DEAD sandbox still holds its volume", err)
	}
}

func TestFailedCreateCleansUpAndUntracks(t *testing.T) {
	h := newHarness(t, func(string) *fakeVM {
		return &fakeVM{createErr: errors.New("no kernel"), exit: make(chan error, 1)}
	})
	if err := h.m.Create(CreateInput{ID: sbx}); err == nil {
		t.Fatal("Create succeeded, want error")
	}
	if _, ok := snapshotState(h.m, sbx); ok {
		t.Fatal("failed sandbox still tracked")
	}
	v := h.vm(sbx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.stopped || !v.cleaned {
		t.Fatalf("stopped=%v cleaned=%v, want both", v.stopped, v.cleaned)
	}
}

func TestStopKeepsSandboxUntilRemove(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.m.Stop(sbx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	v := h.vm(sbx)
	v.mu.Lock()
	stopped, cleaned := v.stopped, v.cleaned
	v.mu.Unlock()
	if !stopped || cleaned {
		t.Fatalf("stopped=%v cleaned=%v, want stopped only", stopped, cleaned)
	}
	if _, ok := snapshotState(h.m, sbx); !ok {
		t.Fatal("stopped sandbox must stay tracked until Remove")
	}

	if err := h.m.Remove(sbx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.cleaned {
		t.Fatal("Remove did not clean up")
	}
}

func TestUnknownSandbox(t *testing.T) {
	h := newHarness(t, runningVM)
	if err := h.m.Stop(sbx); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("Stop = %v, want ErrSandboxNotFound", err)
	}
	if err := h.m.Remove(sbx); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("Remove = %v, want ErrSandboxNotFound", err)
	}
}

// The race the ownership rule exists for: a sandbox booting already has its
// directory and volume on disk. The reconciliation must not reap it.
func TestReconciliationSkipsBootingSandbox(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, func(string) *fakeVM {
		return &fakeVM{state: StateRunning, gate: gate, exit: make(chan error, 1)}
	})
	orphan := "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c09"

	created := make(chan error, 1)
	go func() { created <- h.m.Create(CreateInput{ID: sbx}) }()

	// Wait until the Service is blocked inside vm.Create.
	deadline := time.Now().Add(time.Second)
	for h.vm(sbx) == nil {
		if time.Now().After(deadline) {
			t.Fatal("Create never reached the VM")
		}
		time.Sleep(time.Millisecond)
	}

	h.mu.Lock()
	h.disk = []string{sbx, orphan}
	h.mu.Unlock()
	h.m.reconcileDisk()

	if got := h.reapedIDs(); len(got) != 1 || got[0] != orphan {
		t.Fatalf("reaped = %v, want only [%s]", got, orphan)
	}

	close(gate)
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestCreateDuringReapIsBusy(t *testing.T) {
	h := newHarness(t, runningVM)
	gate := make(chan struct{})
	h.mu.Lock()
	h.disk = []string{sbx}
	h.reapGate = gate
	h.mu.Unlock()

	reconciled := make(chan struct{})
	go func() { h.m.reconcileDisk(); close(reconciled) }()

	deadline := time.Now().Add(time.Second)
	for len(h.reapedIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("reconciliation never started reaping")
		}
		time.Sleep(time.Millisecond)
	}

	if err := h.m.Create(CreateInput{ID: sbx}); !errors.Is(err, ErrBusy) {
		t.Fatalf("Create during reap = %v, want ErrBusy", err)
	}

	close(gate)
	<-reconciled
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create after reap: %v", err)
	}
}

func TestFullEventBufferDoesNotBlockServices(t *testing.T) {
	h := newHarness(t, runningVM)
	for range cap(h.m.events) {
		h.m.events <- StateChange{}
	}

	done := make(chan error, 1)
	go func() { done <- h.m.Create(CreateInput{ID: sbx}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Create blocked on a full event buffer")
	}
	// The state is still recorded for the heartbeat.
	if st, _ := snapshotState(h.m, sbx); st != StateRunning {
		t.Fatalf("snapshot state = %v, want RUNNING", st)
	}
}

// A restarted worker takes over the VMs whose shim survived, in their current
// state, and reaps whatever it cannot attach to.
func TestRecoverAdoptsLiveShimsAndReapsTheRest(t *testing.T) {
	h := newHarness(t, runningVM)
	live := runningVM(sbx)
	lost := "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c05"
	unreachable := "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c06"
	unknownVM := &fakeVM{stateErr: errors.New("api timeout"), exit: make(chan error, 1)}
	h.mu.Lock()
	h.disk = []string{sbx, lost, unreachable}
	h.alive[sbx] = live
	h.alive[unreachable] = unknownVM
	h.mu.Unlock()

	n, err := h.m.Recover()
	if err != nil || n != 2 {
		t.Fatalf("Recover = %d, %v; want 2 adopted", n, err)
	}
	if got := h.reapedIDs(); len(got) != 1 || got[0] != lost {
		t.Fatalf("reaped = %v, want only [%s]", got, lost)
	}
	if st, ok := snapshotState(h.m, sbx); !ok || st != StateRunning {
		t.Fatalf("adopted sandbox state = %v (tracked=%v), want RUNNING", st, ok)
	}
	if st, _ := snapshotState(h.m, unreachable); st != StateUnknown {
		t.Fatalf("adopted sandbox whose API fails = %v, want UNKNOWN", st)
	}

	// The adopted VM is watched like any other: its exit turns it DEAD.
	live.exit <- errors.New("firecracker exited with code 137")
	for {
		ev := nextEvent(t, h.m)
		if ev.SandboxID == sbx && ev.State == StateDead {
			break
		}
	}

	// And removed like any other.
	if err := h.m.Remove(sbx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	if !live.stopped || !live.cleaned {
		t.Fatalf("stopped=%v cleaned=%v, want both", live.stopped, live.cleaned)
	}
}

// An adopted sandbox counts against capacity and makes Create idempotent.
func TestRecoveredSandboxesAreTracked(t *testing.T) {
	h := newHarness(t, runningVM)
	h.mu.Lock()
	h.disk = []string{sbx}
	h.alive[sbx] = runningVM(sbx)
	h.mu.Unlock()
	if _, err := h.m.Recover(); err != nil {
		t.Fatal(err)
	}
	h.m.SetCapacity(1)
	if err := h.m.Create(CreateInput{ID: sbx}); err != nil {
		t.Fatalf("Create of an adopted sandbox = %v, want idempotent nil", err)
	}
	if h.vm(sbx) != nil {
		t.Fatal("Create of an adopted sandbox must not build a new VM")
	}
	if err := h.m.Create(CreateInput{ID: "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c07"}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("Create = %v, want ErrAtCapacity", err)
	}
}
