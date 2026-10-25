package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github/nallanos/fire2/internal/packages/shim"
)

// stopGrace is how long Stop lets the shim stop the VM gracefully before
// killing the shim, which kills the VM.
const stopGrace = 5 * time.Second

// Client drives one sandbox's VM through its shim. The worker is never the
// VM's parent: the shim is, so the VM survives a worker crash or restart.
type Client struct {
	sandboxId string
	Dir       string
	handle    *shim.Handle // nil until Spawn or Attach
	api       *http.Client // Firecracker's API, over its unix socket
}

// SpawnRequest describes the VM to boot.
type SpawnRequest struct {
	ShimPath        string
	FirecrackerPath string

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
// own subdirectory (sockets, vm.json, logs). Configurable via
// FIRECRACKER_SANDBOX_DIR; defaults to /tmp for local dev. Shared with
// ReapOrphans so both agree on where sandbox directories live.
func SandboxBaseDir() string {
	if dir := os.Getenv(sandboxDirEnv); dir != "" {
		return dir
	}
	return defaultSandboxBaseDir
}

// NewClient creates the sandbox's directory, for a VM about to be spawned. The
// ID must be a sandbox ID (a UUID): it names a directory the worker later
// removes recursively, as root.
func NewClient(sandboxId string) (*Client, error) {
	if err := CheckSandboxID(sandboxId); err != nil {
		return nil, err
	}
	dir := filepath.Join(SandboxBaseDir(), sandboxId)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	return newClient(sandboxId, dir), nil
}

// Attach reconnects to the VM a previous worker process left running. It
// fails with shim.ErrShimGone if the sandbox has no live shim, and with
// ErrInvalidSandboxID if the ID is not a sandbox ID.
func Attach(sandboxId string) (*Client, error) {
	if err := CheckSandboxID(sandboxId); err != nil {
		return nil, err
	}
	c := newClient(sandboxId, filepath.Join(SandboxBaseDir(), sandboxId))
	h, err := shim.Attach(c.Dir)
	if err != nil {
		return nil, err
	}
	c.handle = h
	return c, nil
}

func newClient(sandboxId, dir string) *Client {
	return &Client{sandboxId: sandboxId, Dir: dir, api: apiClient(shim.Path(dir, shim.APISocket))}
}

// Spawn writes the VM's config and starts its shim, which boots it right
// away (no API calls). It returns once the shim is up, not once the VM runs:
// poll State for that.
func (c *Client) Spawn(req SpawnRequest) error {
	if c.handle != nil {
		return fmt.Errorf("sandbox %s: already spawned", c.sandboxId)
	}
	if req.RootDrivePath == "" {
		return fmt.Errorf("no root drive")
	}
	if err := writeVMConfig(shim.Path(c.Dir, shim.ConfigFile), req); err != nil {
		return err
	}
	h, err := shim.Spawn(req.ShimPath, c.Dir, req.FirecrackerPath)
	if err != nil {
		return err
	}
	c.handle = h
	return nil
}

// vmConfig is Firecracker's --config-file format.
type vmConfig struct {
	BootSource    bootSource    `json:"boot-source"`
	Drives        []drive       `json:"drives"`
	MachineConfig machineConfig `json:"machine-config"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type machineConfig struct {
	VcpuCount  int32 `json:"vcpu_count"`
	MemSizeMib int32 `json:"mem_size_mib"`
}

func writeVMConfig(path string, req SpawnRequest) error {
	raw, err := json.MarshalIndent(vmConfig{
		BootSource: bootSource{KernelImagePath: req.KernelPath, BootArgs: req.BootArgs},
		Drives: []drive{{
			DriveID:      "rootfs",
			PathOnHost:   req.RootDrivePath,
			IsRootDevice: true,
		}},
		MachineConfig: machineConfig{VcpuCount: req.VcpuCount, MemSizeMib: req.MemSizeMib},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write vm config: %w", err)
	}
	return nil
}

// Stop stops the VM and returns once it is gone. A no-op if it was never
// spawned, so it is safe on a rollback path.
func (c *Client) Stop() error {
	if c.handle == nil {
		return nil
	}
	return c.handle.Stop(stopGrace)
}

// Wait blocks until the VM is gone: Firecracker exited, or its shim died
// (which kills it). It is the only reliable signal that a VM is dead:
// Firecracker's API has no "exited" state, it just stops answering.
func (c *Client) Wait(ctx context.Context) error {
	if c.handle == nil {
		return fmt.Errorf("vm not spawned")
	}
	code, err := c.handle.Wait(ctx)
	if err != nil {
		return err
	}
	return fmt.Errorf("firecracker exited with code %d", code)
}

// Done is closed once the VM is gone; nil before Spawn or Attach.
func (c *Client) Done() <-chan struct{} {
	if c.handle == nil {
		return nil
	}
	return c.handle.Done()
}
