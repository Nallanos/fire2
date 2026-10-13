package sandboxmgr

import (
	"context"
	"log"
	"time"
)

// auditInterval is how often the Manager checks its Services and the disk.
const auditInterval = 30 * time.Second

// Run audits the sandboxes every interval until ctx is done.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = auditInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.audit()
		}
	}
}

func (m *Manager) audit() {
	m.checkLiveness()
	m.reconcileDisk()
}

// checkLiveness marks DEAD every tracked sandbox whose Service goroutine has
// exited. A panicking Service already reports DEAD itself; this catches any
// other way out. The entry stays tracked until the orchestrator removes it.
func (m *Manager) checkLiveness() {
	var dead []string
	m.mu.Lock()
	for id, e := range m.services {
		select {
		case <-e.svc.done:
			if e.state != StateDead {
				e.state = StateDead
				dead = append(dead, id)
			}
		default:
		}
	}
	m.mu.Unlock()
	for _, id := range dead {
		log.Printf("sandbox %s: service goroutine exited, marking dead", id)
		m.emit(id, StateDead)
	}
}

// reconcileDisk reaps every sandbox with a directory in the base dir that no
// one owns. Reaping removes its Firecracker process, volume and directory.
//
// The order matters. The disk is listed first, then compared with the map:
// since Create registers an ID before its Service touches the disk, anything
// seen on disk was already in the map by the time we lock it, unless no one
// owns it. Listing after locking would let a Create slip in between.
func (m *Manager) reconcileDisk() {
	onDisk, err := m.listDisk()
	if err != nil {
		log.Printf("sandbox reconciliation: list disk: %v", err)
		return
	}

	var orphans []string
	m.mu.Lock()
	for _, id := range onDisk {
		if _, ok := m.services[id]; ok {
			continue
		}
		if _, ok := m.deleting[id]; ok {
			continue
		}
		// Claim the ID: a Create for it now gets ErrBusy instead of
		// booting on top of a volume being removed.
		m.deleting[id] = struct{}{}
		orphans = append(orphans, id)
	}
	m.mu.Unlock()

	for _, id := range orphans {
		log.Printf("sandbox reconciliation: reaping orphan %s", id)
		if err := m.reap(id); err != nil {
			log.Printf("sandbox reconciliation: reap %s: %v", id, err)
		}
		m.mu.Lock()
		delete(m.deleting, id)
		m.mu.Unlock()
	}
}
