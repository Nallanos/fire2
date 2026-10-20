package shim

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
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
	_ = os.WriteFile(filepath.Join(filepath.Dir(apiSock), "fc.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)

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
	t.Setenv(helperEnv, "1")
	t.Setenv(fakeModeEnv, mode)
	dir := t.TempDir()
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
			pid, _ := strconv.Atoi(string(raw))
			return pid
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

func TestShimReportsFirecrackerExit(t *testing.T) {
	h, _ := spawn(t, "exit:3")
	code, err := waitDone(t, h)
	if err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v; want 3, nil", code, err)
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
	if _, err := os.Stat(filepath.Join(dir, SocketFile)); !os.IsNotExist(err) {
		t.Fatalf("shim socket left behind: %v", err)
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
