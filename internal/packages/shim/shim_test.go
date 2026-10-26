package shim

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the shim and as a fake Firecracker, depending on
// its arguments, when this variable is set. Children inherit it.
const helperEnv = "FIRE2_SHIM_TEST_HELPER"

// fakeModeEnv tells the fake Firecracker how to behave:
//
//	run          serve the API until SIGTERM, then exit 0 (default)
//	ignore-term  serve the API and ignore SIGTERM
//	exit:<code>  exit with code right away
const fakeModeEnv = "FIRE2_FAKE_FC_MODE"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if slices.Contains(os.Args, "--api-sock") {
			os.Exit(fakeFirecracker(os.Args[1:]))
		}
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeFirecracker(args []string) int {
	var apiSock string
	for i, a := range args {
		if a == "--api-sock" && i+1 < len(args) {
			apiSock = args[i+1]
		}
	}
	// Let tests check Pdeathsig: they look this PID up after killing the shim.
	// Written atomically: a test polling for it must never read it half-written
	// (an empty file reads as PID 0, which is "gone" for every liveness check).
	pidFile := filepath.Join(filepath.Dir(apiSock), "fc.pid")
	if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o644); err == nil {
		_ = os.Rename(pidFile+".tmp", pidFile)
	}

	mode := os.Getenv(fakeModeEnv)
	if code, ok := strings.CutPrefix(mode, "exit:"); ok {
		n, _ := strconv.Atoi(code)
		return n
	}

	terms := make(chan os.Signal, 1)
	if mode == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(terms, syscall.SIGTERM)
	}
	ln, err := net.Listen("unix", apiSock)
	if err != nil {
		return 1
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"fake","state":"Running"}`))
	}))
	<-terms
	return 0
}

func spawn(t *testing.T, mode string) (*Handle, string) {
	t.Helper()
	return spawnIn(t, t.TempDir(), mode)
}

func spawnIn(t *testing.T, dir, mode string) (*Handle, string) {
	t.Helper()
	t.Setenv(helperEnv, "1")
	t.Setenv(fakeModeEnv, mode)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Spawn(self, dir, self)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(h.Pid(), syscall.SIGKILL) })
	return h, dir
}

func waitDone(t *testing.T, h *Handle) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := h.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("VM never reported gone")
	}
	return code, err
}

func fcPid(t *testing.T, dir string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "fc.pid"))
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fake firecracker never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// alive reports whether pid runs (and is not a zombie).
func alive(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	return err == nil && !strings.Contains(string(raw), ") Z ")
}

func processGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Gone, or a zombie waiting for its (dead) parent's reaper.
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// fileGone waits, bounded, for path to disappear.
func fileGone(path string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestShimReportsFirecrackerExit(t *testing.T) {
	h, _ := spawn(t, "exit:3")
	code, err := waitDone(t, h)
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
	}
}

// The shim records Firecracker's exit before telling its clients, so a worker
// that connects too late still learns it.
func TestShimWritesTheExitFile(t *testing.T) {
	h, dir := spawn(t, "exit:3")
	if _, err := waitDone(t, h); err != nil {
		t.Fatal(err)
	}
	if code, err := readExit(dir); err != nil || code != 3 {
		t.Fatalf("readExit = %d, %v; want 3, nil", code, err)
	}
}

// A shim that dies without recording anything must be an error, not the
// leftover exit of a previous run.
func TestSpawnIgnoresAStaleExitFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeExit(dir, 7); err != nil {
		t.Fatal(err)
	}
	dyingShim, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) to stand in for a shim that dies at once")
	}
	if h, err := Spawn(dyingShim, dir, "unused"); err == nil {
		t.Fatalf("Spawn returned a handle (pid %d), want an error", h.Pid())
	}
}

// Whether or not Spawn managed to connect before the VM exited, Close must
// not panic.
func TestCloseAfterAnImmediateExit(t *testing.T) {
	h, _ := spawn(t, "exit:3")
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStopIsGraceful(t *testing.T) {
	h, dir := spawn(t, "run")
	fcPid(t, dir)
	if err := h.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	code, err := waitDone(t, h)
	if err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v; want a clean exit after SIGTERM", code, err)
	}
	// The shim tells its clients first, then closes its listener, which unlinks
	// the socket: the file may outlive Wait by a moment.
	if !fileGone(filepath.Join(dir, SocketFile)) {
		t.Fatal("shim socket left behind")
	}
}

func TestShimKillsAFirecrackerIgnoringSIGTERM(t *testing.T) {
	h, dir := spawn(t, "ignore-term")
	fcPid(t, dir)
	began := time.Now()
	if err := h.Stop(10 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	code, err := waitDone(t, h)
	if err != nil || code != 128+int(syscall.SIGKILL) {
		t.Fatalf("Wait = %d, %v; want SIGKILL after the shim's grace", code, err)
	}
	if elapsed := time.Since(began); elapsed < killGrace {
		t.Fatalf("killed after %s, before the %s grace", elapsed, killGrace)
	}
}

// The point of Pdeathsig: a dead shim takes its VM with it, and the worker
// sees EOF without an exit line.
func TestKillingTheShimKillsTheVM(t *testing.T) {
	h, dir := spawn(t, "run")
	vm := fcPid(t, dir)
	if err := syscall.Kill(h.Pid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err := waitDone(t, h); err == nil {
		t.Fatal("a dead shim must be reported as an error")
	}
	if !processGone(vm) {
		t.Fatalf("firecracker %d survived its shim", vm)
	}
}

// The point of the shim: the worker's connection can go away (worker crash or
// restart) and a new one re-attaches to the same VM.
func TestAttachAfterTheWorkerLeft(t *testing.T) {
	h, dir := spawn(t, "run")
	vm := fcPid(t, dir)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !alive(vm) {
		t.Fatal("the VM died with the worker's connection")
	}

	again, err := Attach(dir)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if again.Pid() != h.Pid() {
		t.Fatalf("attached to pid %d, want %d", again.Pid(), h.Pid())
	}
	if err := again.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if code, err := waitDone(t, again); err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if _, err := Attach(dir); !errors.Is(err, ErrShimGone) {
		t.Fatalf("Attach after exit = %v, want ErrShimGone", err)
	}
}

// A connection the shim never answered is dropped when its listener closes: it
// sat in the socket's queue (Dial succeeded, Accept never ran) while the shim
// finished. The exit is in shim.exit, and that is the code Wait returns, not
// "the shim died".
func TestWaitFallsBackToTheExitFile(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", Path(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("unix", Path(dir, SocketFile)) // queued: nobody accepts
	if err != nil {
		t.Fatal(err)
	}
	h := newHandle(dir, os.Getpid(), conn)

	// The shim's order: record the exit, then drop its clients.
	if err := writeExit(dir, 3); err != nil {
		t.Fatal(err)
	}
	ln.Close()

	if code, err := waitDone(t, h); err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil from shim.exit", code, err)
	}
}

// The same, when what arrives is not an exit line.
func TestWaitFallsBackToTheExitFileOnAnUnreadableLine(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", Path(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_, _ = conn.Write([]byte("garbage\n"))
			conn.Close()
		}
	}()
	if err := writeExit(dir, 4); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", Path(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	if code, err := waitDone(t, newHandle(dir, os.Getpid(), conn)); err != nil || code != 4 {
		t.Fatalf("Wait = %d, %v; want 4, nil from shim.exit", code, err)
	}
}

// Without shim.exit, a connection that drops means the shim died, and the VM
// with it: that must still be an error.
func TestWaitWithoutAnExitFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", Path(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("unix", Path(dir, SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	h := newHandle(dir, os.Getpid(), conn)
	ln.Close()

	if code, err := waitDone(t, h); err == nil || code != -1 {
		t.Fatalf("Wait = %d, %v; want -1 and an error", code, err)
	}
}

// A shim whose VM exited is only finishing: a connection made now may never be
// answered. Attach reports it gone, however alive the process still is.
func TestAttachRefusesAShimWhoseVMExited(t *testing.T) {
	h, dir := spawn(t, "run")
	fcPid(t, dir)
	if err := writeExit(dir, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(dir); !errors.Is(err, ErrShimGone) {
		t.Fatalf("Attach = %v, want ErrShimGone", err)
	}
	if !alive(h.Pid()) {
		t.Fatal("the shim should still be running: this checks Attach does not need it gone")
	}
}

func TestAttachRefusesAForeignPid(t *testing.T) {
	dir := t.TempDir()
	// Our own PID is alive but is not a shim of dir.
	if err := os.WriteFile(filepath.Join(dir, PidFile), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Attach(dir); !errors.Is(err, ErrShimGone) {
		t.Fatalf("Attach = %v, want ErrShimGone", err)
	}
	if _, err := Attach(t.TempDir()); !errors.Is(err, ErrShimGone) {
		t.Fatalf("Attach without pid file = %v, want ErrShimGone", err)
	}
}

func TestParseExitedLine(t *testing.T) {
	if code, err := parseExitedLine(exitedLine(137)); err != nil || code != 137 {
		t.Fatalf("round trip = %d, %v", code, err)
	}
	if _, err := parseExitedLine("hello\n"); err == nil {
		t.Fatal("garbage must be refused")
	}
}
