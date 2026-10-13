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
	// RootDrivePath is the block device holding the sandbox's root
	// filesystem (see internal/packages/thin).
	RootDrivePath string

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
func (c *Client) CreateMachine(req CreateVmRequest) error {
	if req.RootDrivePath == "" {
		return fmt.Errorf("no root drive")
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
				PathOnHost:   firecracker.String(req.RootDrivePath),
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(int64(req.VcpuCount)),
			MemSizeMib: firecracker.Int64(int64(req.MemSizeMib)),
		},
	}

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
func (c *Client) Start() error {
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
func (c *Client) Stop() error {
	if c.machine == nil {
		return nil
	}
	if err := c.machine.StopVMM(); err != nil {
		return fmt.Errorf("stop machine: %w", err)
	}
	return nil
}

// Wait blocks until the Firecracker process exits, or ctx is done. It is the
// only reliable signal that a VM is dead: DescribeInstanceInfo has no
// "exited" state, it just starts failing.
func (c *Client) Wait(ctx context.Context) error {
	if c.machine == nil {
		return fmt.Errorf("machine not created")
	}
	return c.machine.Wait(ctx)
}
