package shim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// connectTimeout bounds how long Spawn waits for a new shim's socket.
const connectTimeout = 3 * time.Second

// ErrShimGone is returned by Attach when the sandbox has no live shim.
var ErrShimGone = errors.New("no live shim")

// Handle is the worker's connection to one shim.
type Handle struct {
	dir  string
	pid  int
	conn net.Conn

	done chan struct{} // closed once the shim reported the exit, or died
	code int           // Firecracker's exit code, valid after done
	err  error         // non-nil if the shim died without reporting, valid after done
}

// Spawn starts a shim for the VM described by dir/vm.json, detached from the
// caller: own session, so it survives the worker. It returns once connected.
func Spawn(shimPath, dir, firecrackerPath string) (*Handle, error) {
	logFile, err := os.OpenFile(Path(dir, ShimLogFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open shim log: %w", err)
	}
	defer logFile.Close()

	// An exit left by a previous run must not pass for this VM's.
	if err := os.Remove(Path(dir, ExitFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale exit file: %w", err)
	}

	cmd := &exec.Cmd{
		Path:   shimPath,
		Args:   []string{ProcessName, "-dir", dir, "-firecracker", firecrackerPath},
		Dir:    dir,
		Stdout: logFile,
		Stderr: logFile,
		// New session: no controlling terminal, out of the worker's
		// process group. Deliberately no Pdeathsig: the shim must outlive
		// the worker.
		SysProcAttr: &syscall.SysProcAttr{Setsid: true},
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}
	// Reap the shim if it exits while this worker still runs; after a
	// worker restart, init reaps it.
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.Now().Add(connectTimeout)
	for {
		conn, err := net.Dial("unix", Path(dir, SocketFile))
		if err == nil {
			return newHandle(dir, cmd.Process.Pid, conn), nil
		}
		select {
		case <-exited:
			// Firecracker can exit before we ever connect: the shim recorded
			// it in a file. No file means the shim itself died.
			code, err := readExit(dir)
			if err != nil {
				return nil, fmt.Errorf("shim exited before accepting connections (see %s): %w", Path(dir, ShimLogFile), err)
			}
			// A handle that is already done, with no connection.
			return &Handle{dir: dir, pid: cmd.Process.Pid, done: exited, code: code}, nil
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("shim did not listen within %s: %w", connectTimeout, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Attach reconnects to the shim already running for dir, e.g. after a worker
// restart. It returns ErrShimGone if there is none, or if its VM already exited:
// the shim is then only finishing, and a connection made now may sit in the
// socket's queue and be dropped unanswered when its listener closes.
func Attach(dir string) (*Handle, error) {
	pid, err := readPid(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShimGone, err)
	}
	if !isShimOf(pid, dir) {
		return nil, fmt.Errorf("%w: pid %d is not the shim of %s", ErrShimGone, pid, dir)
	}
	// shim.exit is written before the shim tells anyone, and Spawn removes the
	// previous one: if it exists, the VM of this shim is over.
	if code, err := readExit(dir); err == nil {
		return nil, fmt.Errorf("%w: its VM already exited with code %d", ErrShimGone, code)
	}
	conn, err := net.Dial("unix", Path(dir, SocketFile))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShimGone, err)
	}
	return newHandle(dir, pid, conn), nil
}

func newHandle(dir string, pid int, conn net.Conn) *Handle {
	h := &Handle{dir: dir, pid: pid, conn: conn, done: make(chan struct{})}
	go h.read()
	return h
}

// read waits for the shim's only message, or for the connection to drop.
//
// A connection can drop without the shim ever having answered it: one still in
// the socket's queue (Dial succeeded, Accept has not run) when the shim, done
// telling its clients, closes its listener. The shim records the exit in
// shim.exit before it tells anyone, so when the line is missing or unreadable
// the file still has the code, if Firecracker did exit. Without it, the shim
// itself died.
func (h *Handle) read() {
	defer close(h.done)
	line, err := bufio.NewReader(h.conn).ReadString('\n')
	h.conn.Close()
	if err == nil {
		var code int
		if code, err = parseExitedLine(line); err == nil {
			h.code = code
			return
		}
	} else {
		err = errors.New("shim died without reporting an exit")
	}
	if code, exitErr := readExit(h.dir); exitErr == nil {
		h.code = code
		return
	}
	h.code, h.err = -1, err
}

// Pid is the shim's PID.
func (h *Handle) Pid() int { return h.pid }

// Done is closed once the VM is gone: Firecracker exited, or the shim died.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Wait blocks until the VM is gone and returns Firecracker's exit code. A
// non-nil error means the shim died without reporting one (the VM died with
// it) or ctx is done.
func (h *Handle) Wait(ctx context.Context) (int, error) {
	select {
	case <-h.done:
		return h.code, h.err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// Stop asks the shim to stop the VM (SIGTERM), and kills the shim if the VM
// is not gone after grace, which kills the VM too. It returns once the VM is
// gone.
func (h *Handle) Stop(grace time.Duration) error {
	select {
	case <-h.done:
		return nil
	default:
	}
	if err := syscall.Kill(h.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal shim %d: %w", h.pid, err)
	}
	select {
	case <-h.done:
		return nil
	case <-time.After(grace):
	}
	if err := syscall.Kill(h.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill shim %d: %w", h.pid, err)
	}
	select {
	case <-h.done:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("shim %d still connected after SIGKILL", h.pid)
	}
}

// Close drops the connection without touching the VM: the shim keeps it
// running for the next worker to attach.
func (h *Handle) Close() error {
	if h.conn == nil {
		return nil // the VM was already gone when Spawn returned: never connected
	}
	return h.conn.Close()
}

// KillStale kills dir's shim if one is still running, which kills its VM
// (Pdeathsig). For reaping a sandbox nobody can attach to.
func KillStale(dir string) error {
	pid, err := readPid(dir)
	if err != nil || !isShimOf(pid, dir) {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill shim %d: %w", pid, err)
	}
	return nil
}

func readPid(dir string) (int, error) {
	raw, err := os.ReadFile(Path(dir, PidFile))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pid file %q", raw)
	}
	return pid, nil
}

// isShimOf reports whether pid is a live shim running for dir: its argv must
// be ProcessName ... -dir <dir>. A PID recycled by another process fails it.
func isShimOf(pid int, dir string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
	if len(args) == 0 || filepath.Base(args[0]) != ProcessName {
		return false
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "-dir" && args[i+1] == dir {
			return true
		}
	}
	return false
}
