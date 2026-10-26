package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/shim"
)

// The test binary doubles as fire2-shim and as a fake Firecracker when this
// variable is set (children inherit it).
const helperEnv = "FIRE2_RUNTIME_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		if slices.Contains(os.Args, "--api-sock") {
			os.Exit(fakeFirecracker(os.Args[1:]))
		}
		os.Exit(shim.Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeFirecracker boots "successfully" if --config-file is a valid VM config,
// serves GET / on the API socket, and exits on SIGTERM.
func fakeFirecracker(args []string) int {
	flag := func(name string) string {
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			return ""
		}
		return args[i+1]
	}
	raw, err := os.ReadFile(flag("--config-file"))
	if err != nil {
		return 2
	}
	var cfg vmConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.BootSource.KernelImagePath == "" ||
		len(cfg.Drives) != 1 || !cfg.Drives[0].IsRootDevice || cfg.MachineConfig.VcpuCount == 0 {
		return 3
	}

	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	ln, err := net.Listen("unix", flag("--api-sock"))
	if err != nil {
		return 1
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"fake","state":"Running","vmm_version":"1.16.1"}`))
	}))
	<-terms
	return 0
}

func waitState(t *testing.T, c *Client, want orchestratorv1.SandboxState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		st, err := c.State(ctx)
		cancel()
		if err == nil && st == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %v, %v; want %v", st, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The reason shims exist: the worker goes away (crash, restart, redeploy),
// the VM keeps running, and the next worker takes it over.
func TestVMSurvivesTheWorker(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv(sandboxDirEnv, t.TempDir())
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const id = "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01"

	first, err := NewClient(id)
	if err != nil {
		t.Fatal(err)
	}
	err = first.Spawn(SpawnRequest{
		ShimPath: self, FirecrackerPath: self,
		KernelPath: "/images/vmlinux", RootDrivePath: "/dev/mapper/fire2-sb-" + id,
		VcpuCount: 1, MemSizeMib: 256, BootArgs: "console=ttyS0",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = first.handle.Stop(time.Second) })
	waitState(t, first, orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING)

	// The worker dies: its connection to the shim goes away.
	if err := first.handle.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Attach(id)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitState(t, second, orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING)

	if err := second.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-second.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VM still there after Stop")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := second.State(ctx); err == nil {
		t.Fatal("a stopped VM must not answer")
	}
	if _, err := Attach(id); err == nil {
		t.Fatal("Attach must fail once the shim is gone")
	}
}

func TestSpawnFailsOnABadConfig(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv(sandboxDirEnv, t.TempDir())
	self, _ := os.Executable()
	c, err := NewClient("6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c02")
	if err != nil {
		t.Fatal(err)
	}
	// VcpuCount 0: the fake Firecracker refuses the config and exits.
	if err := c.Spawn(SpawnRequest{ShimPath: self, FirecrackerPath: self, KernelPath: "/k", RootDrivePath: "/d"}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Wait(ctx); err == nil || err.Error() != "firecracker exited with code 3" {
		t.Fatalf("Wait = %v, want firecracker's exit code", err)
	}
}

// The sandbox ID becomes a path the worker creates and removes recursively as
// root. NewClient and Attach turn away anything that is not a sandbox ID
// before touching the disk.
func TestNewClientAndAttachRefuseAPathAsAnID(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandboxDirEnv, base)

	for _, id := range []string{"../../escaped", "../escaped", "..", ".", "/etc", "a/b", "", "sbx-1"} {
		if c, err := NewClient(id); !errors.Is(err, ErrInvalidSandboxID) || c != nil {
			t.Errorf("NewClient(%q) = %v, %v, want ErrInvalidSandboxID", id, c, err)
		}
		if c, err := Attach(id); !errors.Is(err, ErrInvalidSandboxID) || c != nil {
			t.Errorf("Attach(%q) = %v, %v, want ErrInvalidSandboxID", id, c, err)
		}
	}

	var found []string
	_ = filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && path != root {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 2 {
		t.Errorf("files under %s = %v, want only a and a/b: a refused ID created something", root, found)
	}

	if err := CheckSandboxID("6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01"); err != nil {
		t.Errorf("CheckSandboxID(uuid) = %v", err)
	}
}
