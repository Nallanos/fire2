package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github/nallanos/fire2/internal/packages/shim"
)

// IsSandboxID reports whether name is shaped like a sandbox ID (the
// orchestrator mints UUIDs). Everything that lists sandboxes on disk filters
// with it: the base dir defaults to /tmp, and a reaper must never touch a
// directory it did not create.
func IsSandboxID(name string) bool {
	_, err := uuid.Parse(name)
	return err == nil && len(name) == 36
}

// ErrInvalidSandboxID is returned for an ID that is not a sandbox ID (a UUID).
// A sandbox ID becomes a path under the base dir, which the worker, running as
// root, creates and later removes recursively: an ID such as "../../etc" must
// never get that far.
var ErrInvalidSandboxID = errors.New("invalid sandbox id")

// CheckSandboxID returns nil if id is shaped like a sandbox ID (see
// IsSandboxID), and an error wrapping ErrInvalidSandboxID otherwise.
func CheckSandboxID(id string) error {
	if !IsSandboxID(id) {
		return fmt.Errorf("%w: %q", ErrInvalidSandboxID, id)
	}
	return nil
}

// ListSandboxDirs returns the sandbox IDs that have a directory under baseDir.
func ListSandboxDirs(baseDir string) ([]string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sandbox base dir: %w", err)
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && IsSandboxID(entry.Name()) {
			ids = append(ids, entry.Name())
		}
	}
	return ids, nil
}

// ReapSandbox tears down whatever a sandbox left on this host: it kills the
// sandbox's shim and the Firecracker process still holding its socket,
// removes its thin volume, then its directory. Every step tolerates the resource being already
// gone, so it is safe on a partially created or partially removed sandbox.
//
// The directory is the ownership marker: it is created before the volume and
// removed after it, so a volume only exists while its directory does. If the
// volume cannot be removed, the directory is kept for the next attempt to
// find. Nothing reaps a volume by listing the host-wide device-mapper
// namespace: the directory alone says which sandboxes exist.
//
// removeVolume removes the sandbox's thin volume (thin.Pool.RemoveVolume) and
// must tolerate it being already gone.
//
// The caller must own the ID (see sandboxmgr's ownership rule): reaping a
// sandbox that is being created destroys it.
func ReapSandbox(baseDir, sandboxID string, removeVolume func(sandboxID string) error) error {
	if !IsSandboxID(sandboxID) {
		return fmt.Errorf("refusing to reap %q: not a sandbox id", sandboxID)
	}
	dir := filepath.Join(baseDir, sandboxID)
	socketPath := shim.Path(dir, shim.APISocket)

	// A shim nobody can attach to: killing it kills its VM (Pdeathsig).
	if err := shim.KillStale(dir); err != nil {
		return err
	}
	// A Firecracker still holding the socket: its shim just died and the
	// kernel has not delivered Pdeathsig yet, or it predates shims.
	if pid, found := findProcessHoldingSocket(socketPath); found {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("kill orphaned firecracker pid=%d for %s: %w", pid, sandboxID, err)
		}
		waitForSocketGone(socketPath, 2*time.Second)
	}

	if err := removeVolume(sandboxID); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove orphaned sandbox dir %s: %w", dir, err)
	}
	return nil
}

// ReapOrphans reaps every sandbox in baseDir, live shims and VMs included. It
// backs `worker -reap-all`: stop everything for maintenance. A worker starting
// up does not call it: it re-attaches to live shims instead (see
// sandboxmgr.Manager.Recover) and reaps only what it cannot attach to.
//
// The caller must hold the base dir lock (sandboxmgr.LockBaseDir), so no
// worker owns any of it.
func ReapOrphans(baseDir string, removeVolume func(sandboxID string) error) error {
	ids, err := ListSandboxDirs(baseDir)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := ReapSandbox(baseDir, id, removeVolume); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// findProcessHoldingSocket scans /proc for a firecracker process whose
// --api-sock argument matches socketPath. Returns (0, false) if none is
// found, including on platforms without /proc.
func findProcessHoldingSocket(socketPath string) (pid int, found bool) {
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}

	for _, e := range procEntries {
		p, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID directory
		}

		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // process gone, or no permission — skip
		}

		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(args) == 0 || !strings.Contains(args[0], "firecracker") {
			continue
		}

		for i, a := range args {
			if a == "--api-sock" && i+1 < len(args) && args[i+1] == socketPath {
				return p, true
			}
		}
	}
	return 0, false
}

// waitForSocketGone polls until nothing accepts connections on socketPath,
// or timeout elapses. Best-effort — RemoveAll is safe to call either way.
func waitForSocketGone(socketPath string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
}
