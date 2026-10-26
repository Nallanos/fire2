package worker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
)

// notSandboxIDs are IDs a caller of the unauthenticated gRPC API could send. The
// worker runs as root and an ID becomes a directory it creates and removes
// recursively: none of these may get past the door.
var notSandboxIDs = []string{
	"../../x",
	"../escape",
	"..",
	".",
	"/etc",
	"a/b",
	"",
	"not-a-uuid",
	"6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01/../..",
	"6f1c2a5258a44f4e9a5f0d1d2f6b7c01",       // a UUID without dashes
	"{6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01}", // a UUID in braces
	"6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c0\x00",
}

type countingManager struct {
	sandboxManagerStub
	calls int
}

func (m *countingManager) Create(sandboxmgr.CreateInput) error { m.calls++; return nil }
func (m *countingManager) Stop(string) error                   { m.calls++; return nil }
func (m *countingManager) Remove(string) error                 { m.calls++; return nil }

// CreateSandbox, StopSandbox and RemoveSandbox turn an ID that is not a UUID
// away with InvalidArgument, before the sandbox manager sees it.
func TestGRPCRefusesAnIDThatIsNotASandboxID(t *testing.T) {
	mgr := &countingManager{}
	srv := NewWorkerGRPCServer(NewWorkerService(&heartbeatClientStub{}, mgr))
	ctx := context.Background()

	for _, id := range notSandboxIDs {
		_, err := srv.CreateSandbox(ctx, &workerv1.CreateSandboxRequest{Id: id})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateSandbox(%q) = %v, want InvalidArgument", id, err)
		}
		_, err = srv.StopSandbox(ctx, &workerv1.StopSandboxRequest{ContainerId: id})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("StopSandbox(%q) = %v, want InvalidArgument", id, err)
		}
		_, err = srv.RemoveSandbox(ctx, &workerv1.RemoveSandboxRequest{ContainerId: id})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("RemoveSandbox(%q) = %v, want InvalidArgument", id, err)
		}
	}
	if mgr.calls != 0 {
		t.Errorf("the sandbox manager was called %d time(s) for IDs that are not sandbox IDs", mgr.calls)
	}

	// A UUID goes through, in either case.
	for _, id := range []string{"6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01", "6F1C2A52-58A4-4F4E-9A5F-0D1D2F6B7C01"} {
		if _, err := srv.CreateSandbox(ctx, &workerv1.CreateSandboxRequest{Id: id}); err != nil {
			t.Errorf("CreateSandbox(%q) = %v, want it accepted", id, err)
		}
	}
	if mgr.calls != 2 {
		t.Errorf("manager calls = %d, want 2", mgr.calls)
	}
}

// The same through the real sandbox manager and its base dir: "../../x" is
// refused and no directory appears, in the base dir or anywhere above it. This
// is what the check is for; before it, the only thing standing between a
// crafted ID and `rm -rf` as root was the volume removal failing first.
func TestGRPCCreatesNoDirectoryForAnIDThatIsNotASandboxID(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRECRACKER_SANDBOX_DIR", base)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := sandboxmgr.New(ctx, sandboxmgr.ConfigFromEnv())
	srv := NewWorkerGRPCServer(NewWorkerService(&heartbeatClientStub{}, mgr))

	for _, id := range notSandboxIDs {
		if _, err := srv.CreateSandbox(ctx, &workerv1.CreateSandboxRequest{Id: id}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateSandbox(%q) = %v, want InvalidArgument", id, err)
		}
		if _, err := srv.RemoveSandbox(ctx, &workerv1.RemoveSandboxRequest{ContainerId: id}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("RemoveSandbox(%q) = %v, want InvalidArgument", id, err)
		}
	}

	var found []string
	_ = filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && path != root {
			found = append(found, path)
		}
		return nil
	})
	if want := []string{filepath.Join(root, "a"), base}; len(found) != len(want) || found[0] != want[0] || found[1] != want[1] {
		t.Errorf("files under %s = %v, want only %v: a refused ID created something", root, found, want)
	}
	if got := mgr.Snapshot(); len(got) != 0 {
		t.Errorf("the manager tracks %d sandbox(es) after refused IDs", len(got))
	}
}

// The worker enforces the slot size too, whatever the orchestrator checked: its
// capacity counts slots, and a 4 GiB sandbox would pass for one on a worker of
// 1 GiB without swap.
func TestGRPCRefusesASandboxLargerThanASlot(t *testing.T) {
	mgr := &countingManager{}
	srv := NewWorkerGRPCServer(NewWorkerService(&heartbeatClientStub{}, mgr))
	const id = "6f1c2a52-58a4-4f4e-9a5f-0d1d2f6b7c01"

	for name, req := range map[string]*workerv1.CreateSandboxRequest{
		"4 GiB":   {Id: id, VcpuCount: SandboxVcpu, MemSizeMib: 4096},
		"2 vCPUs": {Id: id, VcpuCount: SandboxVcpu + 1, MemSizeMib: SandboxMemMiB},
	} {
		if _, err := srv.CreateSandbox(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: CreateSandbox = %v, want InvalidArgument", name, err)
		}
	}
	if mgr.calls != 0 {
		t.Errorf("the sandbox manager was called %d time(s) for oversized sandboxes", mgr.calls)
	}

	if _, err := srv.CreateSandbox(context.Background(), &workerv1.CreateSandboxRequest{Id: id, VcpuCount: SandboxVcpu, MemSizeMib: SandboxMemMiB}); err != nil {
		t.Errorf("a sandbox of exactly one slot: CreateSandbox = %v, want it accepted", err)
	}
}
