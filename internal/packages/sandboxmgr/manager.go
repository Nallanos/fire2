package sandboxmgr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
)

// eventBuffer is how many state changes may wait for the worker to forward
// them. When it is full, changes are dropped: events are best-effort, the
// heartbeat inventory carries every sandbox's state anyway.
const eventBuffer = 256

// Ownership rule: an ID present in services or deleting owns its resources on
// disk (directory, thin volume, Firecracker process). Nothing else touches
// them. Create registers the ID before its Service touches the disk, so the
// reconciliation never reaps a sandbox that is booting.
type Manager struct {
	ctx context.Context // lifetime of every Service

	mu       sync.Mutex
	services map[string]*entry
	deleting map[string]struct{} // being removed, or reaped by the reconciliation
	capacity int                 // 0 = unknown yet, no limit

	events chan StateChange

	newVM        vmFactory
	attach       vmAttacher
	reap         func(sandboxID string) error
	listDisk     func() ([]string, error)
	pollInterval time.Duration
	now          func() time.Time
}

type entry struct {
	svc   *Service
	state State
}

// New builds a Manager whose Services boot Firecracker VMs described by cfg
// and live until ctx is done. Call Recover once, then Run to start the audit
// loop.
func New(ctx context.Context, cfg Config) *Manager {
	return newManager(ctx, cfg.newFirecrackerVM, cfg.attach, cfg.reap, cfg.listDisk)
}

func newManager(ctx context.Context, newVM vmFactory, attach vmAttacher, reap func(string) error, listDisk func() ([]string, error)) *Manager {
	return &Manager{
		ctx:          ctx,
		services:     make(map[string]*entry),
		deleting:     make(map[string]struct{}),
		events:       make(chan StateChange, eventBuffer),
		newVM:        newVM,
		attach:       attach,
		reap:         reap,
		listDisk:     listDisk,
		pollInterval: statePollInterval,
		now:          time.Now,
	}
}

// Recover takes over the sandboxes a previous worker process left on this
// host: each one whose shim is still alive is adopted with its VM's current
// state, everything else is reaped. Call it once at startup, after taking the
// base dir lock and before serving requests or sending heartbeats, so the
// first inventory of this incarnation lists the adopted sandboxes (and the
// orchestrator does not fail them as lost). It returns how many it adopted.
func (m *Manager) Recover() (int, error) {
	ids, err := m.listDisk()
	if err != nil {
		return 0, fmt.Errorf("list sandboxes on disk: %w", err)
	}
	adopted := 0
	for _, id := range ids {
		v, err := m.attach(id)
		if err != nil {
			log.Printf("sandbox %s: not recoverable (%v), reaping", id, err)
			if err := m.reap(id); err != nil {
				log.Printf("sandbox %s: reap: %v", id, err)
			}
			continue
		}
		qctx, cancel := context.WithTimeout(m.ctx, stateQueryTimeout)
		st, err := v.State(qctx)
		cancel()
		if err != nil {
			st = StateUnknown
		}
		m.adopt(id, v, st)
		log.Printf("sandbox %s: recovered, %s", id, st)
		adopted++
	}
	return adopted, nil
}

func (m *Manager) adopt(id string, v vm, st State) {
	var svc *Service
	svc = newService(id, m.newVM, func(st State) { m.report(id, svc, st) }, m.pollInterval)
	svc.adopt(m.ctx, v, st)
	m.mu.Lock()
	m.services[id] = &entry{svc: svc, state: st}
	m.mu.Unlock()
	go svc.run(m.ctx)
}

// SetCapacity updates how many sandboxes this worker may host. The worker
// recomputes it on every heartbeat.
func (m *Manager) SetCapacity(capacity int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capacity = capacity
}

// Events returns the stream of state changes. Read it continuously: changes
// are dropped while the buffer is full.
func (m *Manager) Events() <-chan StateChange {
	return m.events
}

// Create boots a sandbox. It is idempotent: a sandbox already tracked is left
// as is, and a duplicate call doesn't count against capacity.
func (m *Manager) Create(in CreateInput) error {
	m.mu.Lock()
	if _, ok := m.services[in.ID]; ok {
		m.mu.Unlock()
		return nil
	}
	if _, ok := m.deleting[in.ID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrBusy, in.ID)
	}
	// Sandboxes being removed still hold their volume: they count too.
	hosted := len(m.services) + len(m.deleting)
	if m.capacity > 0 && hosted >= m.capacity {
		m.mu.Unlock()
		log.Printf("worker at capacity: hosted=%d capacity=%d", hosted, m.capacity)
		return ErrAtCapacity
	}

	// Register before anything exists on disk (see the ownership rule).
	var svc *Service
	svc = newService(in.ID, m.newVM, func(st State) { m.report(in.ID, svc, st) }, m.pollInterval)
	m.services[in.ID] = &entry{svc: svc, state: StateStarting}
	m.mu.Unlock()

	go svc.run(m.ctx)

	if err := svc.do(command{kind: cmdCreate, in: in}); err != nil {
		// The Service cleaned up after itself and exited. A concurrent
		// Remove may already have moved the ID to deleting: only drop the
		// entry if it is still ours.
		m.mu.Lock()
		if e, ok := m.services[in.ID]; ok && e.svc == svc {
			delete(m.services, in.ID)
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

// Stop kills the sandbox's VM. The sandbox stays tracked, with its directory
// and volume, until Remove; its state turns DEAD once the process exits.
func (m *Manager) Stop(sandboxID string) error {
	m.mu.Lock()
	e, ok := m.services[sandboxID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxID)
	}
	err := e.svc.do(command{kind: cmdStop})
	if errors.Is(err, errServiceExited) {
		return nil // nothing left running
	}
	return err
}

// Remove stops the sandbox and releases its resources. It is the only path
// that drops a tracked sandbox, DEAD ones included.
func (m *Manager) Remove(sandboxID string) error {
	m.mu.Lock()
	if _, ok := m.deleting[sandboxID]; ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrBusy, sandboxID)
	}
	e, ok := m.services[sandboxID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxID)
	}
	delete(m.services, sandboxID)
	m.deleting[sandboxID] = struct{}{}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.deleting, sandboxID)
		m.mu.Unlock()
	}()

	err := e.svc.do(command{kind: cmdRemove})
	if errors.Is(err, errServiceExited) {
		// The actor is gone (panic, or shutdown): nothing owns the VM
		// anymore. Reap it straight from disk.
		return m.reap(sandboxID)
	}
	return err
}

// Snapshot returns every tracked sandbox with its last known state, for the
// heartbeat inventory. Sandboxes being removed are left out.
func (m *Manager) Snapshot() []*orchestratorv1.SandboxInventoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*orchestratorv1.SandboxInventoryEntry, 0, len(m.services))
	for id, e := range m.services {
		out = append(out, &orchestratorv1.SandboxInventoryEntry{SandboxId: id, State: e.state})
	}
	return out
}

// report records a Service's new state and forwards it on the event stream.
// It is called from the Service's goroutine.
func (m *Manager) report(sandboxID string, svc *Service, st State) {
	m.mu.Lock()
	e, ok := m.services[sandboxID]
	if !ok || e.svc != svc {
		m.mu.Unlock()
		return // being removed: the orchestrator no longer cares
	}
	e.state = st
	m.mu.Unlock()
	m.emit(sandboxID, st)
}

// emit never blocks: a Service must not stall because nobody reads events.
func (m *Manager) emit(sandboxID string, st State) {
	select {
	case m.events <- StateChange{SandboxID: sandboxID, State: st, At: m.now().UTC()}:
	default:
		log.Printf("sandbox %s: event buffer full, dropping state %s", sandboxID, st)
	}
}
