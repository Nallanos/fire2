package runtime

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ReapOrphans scans baseDir for leftover per-sandbox directories from a
// previous worker process (crash or restart) and removes them, killing any
// Firecracker process still holding the directory's socket first.
//
// The worker keeps no persisted state of its own — sandboxes.go's
// runningSandboxes map is wiped on restart by design. Re-creating a sandbox
// is the orchestrator's job (its existing retry policy re-drives
// stepStartContainer when the worker-side call fails); this function's only
// responsibility is guaranteeing a clean slate for that retry to land on,
// not resuming management of a VM this process didn't start.
//
// Call this once at worker startup, before serving gRPC — otherwise an
// incoming CreateSandbox could race a still-in-progress reap of the same
// directory.
func ReapOrphans(baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read sandbox base dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, entry.Name())
		socketPath := filepath.Join(dir, "firecracker.sock")

		if pid, found := findProcessHoldingSocket(socketPath); found {
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("kill orphaned firecracker pid=%d for %s: %w", pid, entry.Name(), err)
			}
			waitForSocketGone(socketPath, 2*time.Second)
		}

		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove orphaned sandbox dir %s: %w", dir, err)
		}
	}
	return nil
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
