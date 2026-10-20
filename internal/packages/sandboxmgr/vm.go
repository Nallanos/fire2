package sandboxmgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	"github/nallanos/fire2/internal/packages/thin"
)

// vm is the part of a Firecracker VM a Service drives. Tests swap in a fake.
type vm interface {
	// Create provisions the VM's resources and boots it.
	Create(ctx context.Context) error
	// Stop kills the VM. A no-op if it never started.
	Stop() error
	// Wait blocks until the VM's process exits.
	Wait(ctx context.Context) error
	// State samples the VM's state. An error means it could not be queried.
	State(ctx context.Context) (State, error)
	// Cleanup releases the VM's resources on disk. Call it after Stop.
	Cleanup() error
}

// vmFactory builds the vm of one sandbox. It may touch the disk, so it only
// runs inside the sandbox's Service, once the ID is registered in the map.
type vmFactory func(in CreateInput) (vm, error)

const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// startTimeout bounds booting a VM. The SDK bounds each of its steps (3s for
// the API socket, 500ms per API call), but nothing bounds the whole: a stuck
// start would block the sandbox's actor, and with it Remove.
const startTimeout = 30 * time.Second

// Config locates what the worker needs on disk to boot Firecracker VMs.
type Config struct {
	ImageDir     string // holds vmlinux
	ThinPoolConf string // thinpool.conf of the pool holding sandbox root drives
	BaseDir      string // per-sandbox directories (socket); locked by one worker
}

// ConfigFromEnv reads FIRECRACKER_IMAGE_DIR, FIRE2_THINPOOL_CONF and the
// sandbox base dir once. Missing values are reported when a sandbox is
// created, so a worker without Firecracker can still start (local dev).
func ConfigFromEnv() Config {
	thinPoolConf := os.Getenv("FIRE2_THINPOOL_CONF")
	if thinPoolConf == "" {
		thinPoolConf = thin.DefaultConfPath
	}
	return Config{
		ImageDir:     os.Getenv("FIRECRACKER_IMAGE_DIR"),
		ThinPoolConf: thinPoolConf,
		BaseDir:      runtimeClient.SandboxBaseDir(),
	}
}

func (c Config) kernelPath() (string, error) {
	if c.ImageDir == "" {
		return "", errors.New("FIRECRACKER_IMAGE_DIR environment variable is not set")
	}
	kernelPath := filepath.Join(c.ImageDir, "vmlinux")
	if _, err := os.Stat(kernelPath); err != nil {
		return "", fmt.Errorf("vmlinux not found in FIRECRACKER_IMAGE_DIR: %w", err)
	}
	return kernelPath, nil
}

// pool reads the thin pool settings. It is a few-line file, read on each use
// rather than at startup so a worker without a pool can still start.
func (c Config) pool() (thin.Pool, error) {
	return thin.LoadPool(c.ThinPoolConf)
}

// RemoveVolume removes a sandbox's thin volume; a volume already gone is not
// an error.
func (c Config) RemoveVolume(sandboxID string) error {
	pool, err := c.pool()
	if err != nil {
		return err
	}
	return pool.RemoveVolume(sandboxID)
}

// newFirecrackerVM is the production vmFactory.
func (c Config) newFirecrackerVM(in CreateInput) (vm, error) {
	kernelPath, err := c.kernelPath()
	if err != nil {
		return nil, err
	}
	pool, err := c.pool()
	if err != nil {
		return nil, err
	}
	// Creates the sandbox directory: it must exist before the volume (see
	// runtime.ReapSandbox).
	client, err := runtimeClient.NewClient(in.ID)
	if err != nil {
		return nil, fmt.Errorf("create runtime client for sandbox %s: %w", in.ID, err)
	}
	return &firecrackerVM{client: client, pool: pool, kernelPath: kernelPath, in: in}, nil
}

// reap tears down whatever sandboxID left on disk, for IDs no Service owns.
func (c Config) reap(sandboxID string) error {
	return runtimeClient.ReapSandbox(c.BaseDir, sandboxID, c.RemoveVolume)
}

// listDisk returns every sandbox ID with a directory in this worker's base
// dir. The directory is the ownership marker (see runtime.ReapSandbox).
func (c Config) listDisk() ([]string, error) {
	return runtimeClient.ListSandboxDirs(c.BaseDir)
}

type firecrackerVM struct {
	client     *runtimeClient.Client
	pool       thin.Pool
	kernelPath string
	in         CreateInput
}

func (f *firecrackerVM) Create(ctx context.Context) error {
	rootDrive, err := f.pool.CreateVolume(f.in.ID)
	if err != nil {
		return err
	}
	if err := f.client.CreateMachine(runtimeClient.CreateVmRequest{
		KernelPath:    f.kernelPath,
		RootDrivePath: rootDrive,
		VcpuCount:     f.in.VcpuCount,
		MemSizeMib:    f.in.MemSizeMib,
		BootArgs:      defaultBootArgs,
	}); err != nil {
		return err
	}
	return startBounded(ctx, f.client.Start, f.client.Stop, startTimeout)
}

// startBounded runs start, and if it has not returned after timeout (or ctx
// is done first, e.g. worker shutdown), kills the VM with stop so start fails
// and returns. It always waits for start to return: the caller keeps sole
// ownership of the VM, and cleans up as after any failed create.
//
// A deadline cannot simply be passed to the SDK's Machine.Start: it stops the
// VM whenever that ctx is done, so cancelling it after a successful start
// would kill the VM (see runtime.Client.Start).
func startBounded(ctx context.Context, start, stop func() error, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- start() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var cause error
	select {
	case err := <-done:
		return err
	case <-timer.C:
		cause = fmt.Errorf("start timed out after %s", timeout)
	case <-ctx.Done():
		cause = fmt.Errorf("start aborted: %w", ctx.Err())
	}

	stopErr := stop()
	startErr := <-done
	if startErr == nil {
		// Start won the race against stop: the VM booted, then got killed.
		startErr = errors.New("vm stopped after start")
	}
	return errors.Join(cause, stopErr, startErr)
}

func (f *firecrackerVM) Stop() error                              { return f.client.Stop() }
func (f *firecrackerVM) Wait(ctx context.Context) error           { return f.client.Wait(ctx) }
func (f *firecrackerVM) State(ctx context.Context) (State, error) { return f.client.State(ctx) }

// Cleanup removes the volume, then the directory. The directory is kept if
// the volume could not be removed, so the reconciliation finds it again once
// the sandbox is untracked.
func (f *firecrackerVM) Cleanup() error {
	if err := f.pool.RemoveVolume(f.in.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(f.client.Dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sandbox dir: %w", err)
	}
	return nil
}
