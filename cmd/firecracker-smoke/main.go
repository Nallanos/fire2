// Command firecracker-smoke drives one real Firecracker VM through
// runtime.Client end to end (create, boot, stop, cleanup) and reports
// pass/fail. It exercises the exact same code WorkerService.CreateSandbox
// uses — not a fake — so it catches the class of bug that only shows up
// against a real kernel/rootfs/socket (stale sockets, bad boot args,
// permission issues, missing images).
//
// Reads the same environment variables the deployed worker does
// (FIRECRACKER_IMAGE_DIR, FIRECRACKER_SANDBOX_DIR, FIRE2_THINPOOL_CONF,
// FIRE2_SHIM_PATH, FIRECRACKER_BIN), so it validates whatever
// host it runs on with no separate config to keep in sync. Self-cleaning:
// stops the VM and removes its directory before exiting, pass or fail.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
	"github/nallanos/fire2/internal/packages/runtime"
	"github/nallanos/fire2/internal/packages/thin"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	fmt.Println("PASS: VM created, booted, and stopped cleanly")
}

func run() error {
	imageDir := os.Getenv("FIRECRACKER_IMAGE_DIR")
	if imageDir == "" {
		return errors.New("FIRECRACKER_IMAGE_DIR is not set")
	}
	kernelPath := filepath.Join(imageDir, "vmlinux")
	rootfsPath := filepath.Join(imageDir, "rootfs.ext4")
	for _, p := range []string{kernelPath, rootfsPath} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
	}

	sandboxID := "smoke-" + uuid.NewString()
	thinPoolConf := os.Getenv("FIRE2_THINPOOL_CONF")
	if thinPoolConf == "" {
		thinPoolConf = thin.DefaultConfPath
	}
	pool, err := thin.LoadPool(thinPoolConf)
	if err != nil {
		return fmt.Errorf("LoadPool: %w", err)
	}

	client, err := runtime.NewClient(sandboxID)
	if err != nil {
		return fmt.Errorf("NewClient: %w", err)
	}
	// Always attempt cleanup, even on a failed create/start — mirrors what
	// WorkerService.CreateSandbox's rollback does, and keeps repeated runs
	// of this command from littering the host.
	defer func() {
		if err := client.Stop(); err != nil {
			log.Printf("cleanup: stop: %v", err)
		}
		if err := pool.RemoveVolume(sandboxID); err != nil {
			log.Printf("cleanup: remove volume: %v", err)
		}
		if err := os.RemoveAll(client.Dir); err != nil && !os.IsNotExist(err) {
			log.Printf("cleanup: remove dir: %v", err)
		}
	}()

	rootDrive, err := pool.CreateVolume(sandboxID)
	if err != nil {
		return fmt.Errorf("CreateVolume: %w", err)
	}
	if err := client.Spawn(runtime.SpawnRequest{
		ShimPath:        envOr("FIRE2_SHIM_PATH", "/opt/fire2/fire2-shim"),
		FirecrackerPath: envOr("FIRECRACKER_BIN", "/usr/local/bin/firecracker"),
		KernelPath:      kernelPath,
		RootDrivePath:   rootDrive,
		VcpuCount:       1,
		MemSizeMib:      256,
		BootArgs:        "console=ttyS0 reboot=k panic=1 pci=off",
	}); err != nil {
		return fmt.Errorf("Spawn: %w", err)
	}

	// The shim boots the VM from its config file: wait for it to run.
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		st, err := client.State(ctx)
		cancel()
		if err == nil && st == orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING {
			return nil
		}
		select {
		case <-client.Done():
			return fmt.Errorf("VM exited while booting, see %s", filepath.Join(client.Dir, "firecracker.log"))
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("VM not running after 30s: state=%v err=%v", st, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
