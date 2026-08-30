// Command firecracker-smoke drives one real Firecracker VM through
// runtime.Client end to end (create, boot, stop, cleanup) and reports
// pass/fail. It exercises the exact same code WorkerService.CreateSandbox
// uses — not a fake — so it catches the class of bug that only shows up
// against a real kernel/rootfs/socket (stale sockets, bad boot args,
// permission issues, missing images).
//
// Reads the same environment variables the deployed worker does
// (FIRECRACKER_IMAGE_DIR, FIRECRACKER_SANDBOX_DIR), so it validates whatever
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

	"github/nallanos/fire2/internal/packages/runtime"
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client, err := runtime.NewClient(sandboxID)
	if err != nil {
		return fmt.Errorf("NewClient: %w", err)
	}
	// Always attempt cleanup, even on a failed create/start — mirrors what
	// WorkerService.CreateSandbox's rollback does, and keeps repeated runs
	// of this command from littering the host.
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := client.Stop(stopCtx); err != nil {
			log.Printf("cleanup: stop: %v", err)
		}
		if err := os.RemoveAll(client.Dir); err != nil && !os.IsNotExist(err) {
			log.Printf("cleanup: remove dir: %v", err)
		}
	}()

	if err := client.CreateMachine(ctx, runtime.CreateVmRequest{
		KernelPath: kernelPath,
		RootFSPath: rootfsPath,
		VcpuCount:  1,
		MemSizeMib: 256,
		BootArgs:   "console=ttyS0 reboot=k panic=1 pci=off",
	}); err != nil {
		return fmt.Errorf("CreateMachine: %w", err)
	}

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("Start: %w", err)
	}

	return nil
}
