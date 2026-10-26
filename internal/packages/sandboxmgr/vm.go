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
	// Stop kills the VM and returns once it is gone. A no-op if it never
	// started.
	Stop() error
	// Wait blocks until the VM is gone.
	Wait(ctx context.Context) error
	// State samples the VM's state. An error means it could not be queried.
	State(ctx context.Context) (State, error)
	// Cleanup releases the VM's resources on disk. Call it after Stop.
	Cleanup() error
}

// vmFactory builds the vm of one sandbox. It may touch the disk, so it only
// runs inside the sandbox's Service, once the ID is registered in the map.
type vmFactory func(in CreateInput) (vm, error)

// vmAttacher re-attaches to the vm of a sandbox a previous worker process
// left running. It fails if the sandbox has no live shim.
type vmAttacher func(sandboxID string) (vm, error)

const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

const (
	// bootTimeout bounds how long a new VM may take to report Running.
	// Without it, a VM stuck booting would block its actor, and with it
	// Remove, while being reported STARTING (which refreshes last_seen_at,
	// so liveness never fires).
	bootTimeout = 30 * time.Second
	bootPoll    = 50 * time.Millisecond
)

// Config locates what the worker needs on disk to boot Firecracker VMs.
type Config struct {
	ImageDir        string // holds vmlinux
	ThinPoolConf    string // thinpool.conf of the pool holding sandbox root drives
	BaseDir         string // per-sandbox directories; locked by one worker
	ShimPath        string // fire2-shim binary, one per VM
	FirecrackerPath string // firecracker binary, run by the shim
}

// ConfigFromEnv reads FIRECRACKER_IMAGE_DIR, FIRE2_THINPOOL_CONF,
// FIRE2_SHIM_PATH, FIRECRACKER_BIN and the sandbox base dir once. Missing
// values are reported when a sandbox is created, so a worker without
// Firecracker can still start (local dev).
func ConfigFromEnv() Config {
	return Config{
		ImageDir:        os.Getenv("FIRECRACKER_IMAGE_DIR"),
		ThinPoolConf:    envOr("FIRE2_THINPOOL_CONF", thin.DefaultConfPath),
		BaseDir:         runtimeClient.SandboxBaseDir(),
		ShimPath:        envOr("FIRE2_SHIM_PATH", "/opt/fire2/fire2-shim"),
		FirecrackerPath: envOr("FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
	// Creates the sandbox directory: it must exist before the volume (see
	// runtime.ReapSandbox).
	client, err := runtimeClient.NewClient(in.ID)
	if err != nil {
		return nil, fmt.Errorf("create runtime client for sandbox %s: %w", in.ID, err)
	}
	return &firecrackerVM{client: client, cfg: c, kernelPath: kernelPath, in: in}, nil
}

// attach is the production vmAttacher. It needs nothing but the shim: a
// missing pool config must not make a live VM look orphaned.
func (c Config) attach(sandboxID string) (vm, error) {
	client, err := runtimeClient.Attach(sandboxID)
	if err != nil {
		return nil, err
	}
	return &firecrackerVM{client: client, cfg: c, in: CreateInput{ID: sandboxID}}, nil
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
	cfg        Config
	kernelPath string
	in         CreateInput
}

// Create creates the root volume, spawns the shim (which boots the VM from
// its config file), then waits, bounded, for the VM to run. On failure the
// caller stops and cleans up as after any failed create.
func (f *firecrackerVM) Create(ctx context.Context) error {
	pool, err := f.cfg.pool()
	if err != nil {
		return err
	}
	rootDrive, err := pool.CreateVolume(f.in.ID)
	if err != nil {
		return err
	}
	if err := f.client.Spawn(runtimeClient.SpawnRequest{
		ShimPath:        f.cfg.ShimPath,
		FirecrackerPath: f.cfg.FirecrackerPath,
		KernelPath:      f.kernelPath,
		RootDrivePath:   rootDrive,
		VcpuCount:       f.in.VcpuCount,
		MemSizeMib:      f.in.MemSizeMib,
		BootArgs:        defaultBootArgs,
	}); err != nil {
		return err
	}
	return waitRunning(ctx, f.client.State, f.client.Done(), bootTimeout, bootPoll)
}

func (f *firecrackerVM) Stop() error                              { return f.client.Stop() }
func (f *firecrackerVM) Wait(ctx context.Context) error           { return f.client.Wait(ctx) }
func (f *firecrackerVM) State(ctx context.Context) (State, error) { return f.client.State(ctx) }

// Cleanup removes the volume, then the directory. The directory is kept if
// the volume could not be removed, so the reconciliation finds it again once
// the sandbox is untracked.
func (f *firecrackerVM) Cleanup() error {
	if err := f.cfg.RemoveVolume(f.in.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(f.client.Dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove sandbox dir: %w", err)
	}
	return nil
}

// waitRunning polls state until the VM runs. It fails if the VM is gone
// first (gone closes), after timeout, or once ctx is done (worker shutdown).
func waitRunning(ctx context.Context, state func(context.Context) (State, error), gone <-chan struct{}, timeout, poll time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()

	var last error
	for {
		qctx, cancel := context.WithTimeout(ctx, stateQueryTimeout)
		st, err := state(qctx)
		cancel()
		if err == nil && st == StateRunning {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("state %s", st)
		}
		last = err

		select {
		case <-gone:
			return errors.New("vm exited while booting (see firecracker.log in the sandbox dir)")
		case <-deadline.C:
			return fmt.Errorf("vm not running after %s: %w", timeout, last)
		case <-ctx.Done():
			return fmt.Errorf("boot aborted: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
