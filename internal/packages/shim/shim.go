// Package shim keeps a Firecracker VM alive independently of the worker.
//
// One fire2-shim process runs per VM. The worker spawns it detached (own
// session), and it becomes the direct parent of the Firecracker process. When
// the worker crashes or restarts, the shim and its VM carry on; the new worker
// re-attaches to the shim instead of reaping the VM.
//
// The protocol is deliberately tiny, so old shims keep working across worker
// upgrades: a client connects to the shim's unix socket, and the shim writes a
// single line, "exited <code>\n", when Firecracker exits. EOF without that
// line means the shim died, which kills Firecracker too (Pdeathsig). To stop
// the VM, the worker sends SIGTERM to the shim.
//
// This package must stay small and depend on the standard library and
// x/sys only: the shim is the process that must never crash.
package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Files the shim and the worker share in a sandbox directory.
const (
	ConfigFile  = "vm.json"          // Firecracker --config-file, written by the worker
	PidFile     = "shim.pid"         // the shim's PID
	SocketFile  = "shim.sock"        // the shim's control socket
	APISocket   = "firecracker.sock" // Firecracker's API socket
	VMLogFile   = "firecracker.log"  // Firecracker's stdout/stderr (serial console)
	ShimLogFile = "shim.log"         // the shim's own stdout/stderr
)

// ProcessName is the argv[0] every shim runs with. Attach checks it, so a
// recycled PID can never be mistaken for a shim.
const ProcessName = "fire2-shim"

const exitedPrefix = "exited "

const ExitFile = "shim.exit" // Firecracker's exit code, written before the shim tells its clients

func exitedLine(code int) string {
	return exitedPrefix + strconv.Itoa(code) + "\n"
}

func parseExitedLine(line string) (int, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(exitedPrefix))
	if !ok {
		return 0, fmt.Errorf("unexpected shim message %q", line)
	}
	return strconv.Atoi(strings.TrimSpace(rest))
}

// Path returns the path of one of the shared files in dir.
func Path(dir, file string) string {
	return filepath.Join(dir, file)
}

// writeExit records Firecracker's exit code in dir, atomically: a reader sees
// the whole file or none.
func writeExit(dir string, code int) error {
	tmp := Path(dir, ExitFile+".tmp")
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d", code)), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir, ExitFile))
}

// readExit returns the exit code writeExit recorded. The error wraps
// os.ErrNotExist when the shim wrote none.
func readExit(dir string) (int, error) {
	b, err := os.ReadFile(Path(dir, ExitFile))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
