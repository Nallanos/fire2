package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

type Client struct {
	socketPath string
	sandboxId  string
	machine    *firecracker.Machine
	Dir        string
}

type CreateVmRequest struct {
	KernelPath string
	RootFSPath string

	VcpuCount  int32
	MemSizeMib int32
	BootArgs   string
}

const sandboxDirEnv = "FIRECRACKER_SANDBOX_DIR"
const defaultSandboxBaseDir = "/tmp"

// SandboxBaseDir returns the directory under which every sandbox gets its
// own subdirectory (socket, copied rootfs). Configurable via
// FIRECRACKER_SANDBOX_DIR; defaults to /tmp for local dev. Shared with
// ReapOrphans so both agree on where sandbox directories live.
func SandboxBaseDir() string {
	if dir := os.Getenv(sandboxDirEnv); dir != "" {
		return dir
	}
	return defaultSandboxBaseDir
}

func NewClient(sandboxId string) (*Client, error) {
	dir := filepath.Join(SandboxBaseDir(), sandboxId)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	return &Client{
		sandboxId:  sandboxId,
		socketPath: filepath.Join(dir, "firecracker.sock"),
		Dir:        dir,
	}, nil
}

// CreateMachine spawns a Firecracker process for this client's socket, configures
// it (machine config, boot source, root drive). The machine is not started yet.
func (c *Client) CreateMachine(ctx context.Context, req CreateVmRequest) error {
	// copy root fs in the sandbox dir so we can mount it read-write
	rootFSPath := filepath.Join(c.Dir, "rootfs.ext4")
	if err := CopyFile(req.RootFSPath, rootFSPath); err != nil {
		return fmt.Errorf("copy rootfs: %w", err)
	}

	// Defensive: a stale socket file left by a killed/crashed process would
	// make the upcoming bind fail with "address already in use" even though
	// nothing is actually listening. ReapOrphans handles the common case at
	// worker startup; this covers any leftover socket that slips through.
	if err := os.Remove(c.socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	cfg := firecracker.Config{
		SocketPath:      c.socketPath,
		KernelImagePath: req.KernelPath,
		KernelArgs:      req.BootArgs,
		Drives: []models.Drive{
			{
				DriveID:      firecracker.String("rootfs"),
				PathOnHost:   firecracker.String(rootFSPath),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(int64(req.VcpuCount)),
			MemSizeMib: firecracker.Int64(int64(req.MemSizeMib)),
		},
	}

	// The SDK builds the Firecracker process with exec.CommandContext(ctx, ...)
	// — the process is killed the instant ctx is Done. ctx here is the
	// caller's (ultimately a gRPC request context), which is cancelled the
	// moment CreateSandbox returns, i.e. almost immediately after the VM
	// starts. Use context.Background() so the VM's process lifetime is
	// independent of how long the request that created it took. See the
	// same reasoning on Start below — the SDK has a second, separate
	// context-tied kill switch there.
	machine, err := firecracker.NewMachine(context.Background(), cfg)
	if err != nil {
		return fmt.Errorf("new machine: %w", err)
	}

	c.machine = machine
	return nil
}

// Start the machine and wait for it to be ready. The machine must have been created first.
//
// Deliberately ignores ctx for the underlying SDK call: internally,
// Machine.Start(ctx) spawns a goroutine that calls StopVMM() the instant ctx
// is Done — a second, independent kill switch besides the exec.CommandContext
// one in CreateMachine. Passing a request-scoped ctx here would kill the VM
// as soon as the caller's request finishes. Stopping the VM is Client.Stop's
// job, not something that should happen as a side effect of an unrelated
// context expiring.
func (c *Client) Start(ctx context.Context) error {
	if c.machine == nil {
		return fmt.Errorf("machine not created")
	}
	if err := c.machine.Start(context.Background()); err != nil {
		return fmt.Errorf("start machine: %w", err)
	}
	return nil
}

// Stop the machine. A no-op if the machine was never created — nothing to
// stop is success, not an error, matching the SDK's own StopVMM semantics
// ("don't return an error if the process isn't even running"). This keeps
// Stop safe to call from a rollback path where CreateMachine may have failed
// before the machine ever existed.
func (c *Client) Stop(ctx context.Context) error {
	if c.machine == nil {
		return nil
	}
	if err := c.machine.StopVMM(); err != nil {
		return fmt.Errorf("stop machine: %w", err)
	}
	return nil
}
