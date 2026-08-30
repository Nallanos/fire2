package worker

import (
	"context"
	"errors"
	"fmt"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	"log"
	"os"
	"path/filepath"
)

// CreateSandbox starts a container for the sandbox. It is idempotent: if a container
// with this sandbox ID already exists it is reused; duplicate calls for the same ID
// don't increment the running set or capacity count.
func (w *WorkerService) CreateSandbox(ctx context.Context, in CreateSandboxInput) (err error) {
	w.mu.Lock()
	if _, alreadyRunning := w.runningSandboxes[in.ID]; alreadyRunning {
		w.mu.Unlock()
		return nil // idempotent: already tracking this sandbox
	}
	if len(w.runningSandboxes) >= w.worker.Capacity && w.worker.Capacity > 0 {
		log.Printf("worker at capacity: running=%d capacity=%d", len(w.runningSandboxes), w.worker.Capacity)
		w.mu.Unlock()
		return errors.New("worker at full capacity")
	}
	client := w.runningSandboxes[in.ID]
	// If the client is nil, create a new runtime client for this sandbox
	if client == nil {
		var err error
		client, err = runtimeClient.NewClient(in.ID)
		if err != nil {
			w.mu.Unlock()
			return fmt.Errorf("failed to create runtime client for sandbox %s: %w", in.ID, err)
		}
	}
	w.runningSandboxes[in.ID] = client
	w.mu.Unlock()

	defer func() {
		if err != nil {
			w.RemoveSandbox(ctx, in.ID)
		}
	}()

	// get env variables for firecracker image dir
	imageDir := os.Getenv("FIRECRACKER_IMAGE_DIR")
	if imageDir == "" {
		return errors.New("FIRECRACKER_IMAGE_DIR environment variable is not set")
	}

	// check if the image directory exists
	if _, err := os.Stat(imageDir); os.IsNotExist(err) {
		return errors.New("FIRECRACKER_IMAGE_DIR does not exist: " + imageDir)
	}

	rootfsPath := filepath.Join(imageDir, "rootfs.ext4")
	kernelPath := filepath.Join(imageDir, "vmlinux")

	// check if the rootfs and kernel files exist
	if _, err := os.Stat(rootfsPath); os.IsNotExist(err) {
		return errors.New("rootfs.ext4 does not exist in FIRECRACKER_IMAGE_DIR: " + rootfsPath)
	}
	if _, err := os.Stat(kernelPath); os.IsNotExist(err) {
		return errors.New("vmlinux does not exist in FIRECRACKER_IMAGE_DIR: " + kernelPath)
	}

	// Create machine
	err = client.CreateMachine(ctx, runtimeClient.CreateVmRequest{
		KernelPath: kernelPath,
		RootFSPath: rootfsPath,
		VcpuCount:  in.VcpuCount,
		MemSizeMib: in.MemSizeMib,
		BootArgs:   "console=ttyS0 reboot=k panic=1 pci=off",
	})

	if err != nil {
		return err
	}

	// Start machine
	err = client.Start(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (w *WorkerService) StopSandbox(ctx context.Context, containerID string) error {
	w.mu.Lock()
	client := w.runningSandboxes[containerID]
	if client == nil {
		w.mu.Unlock()
		return fmt.Errorf("sandbox not found")
	}
	if err := client.Stop(ctx); err != nil {
		w.mu.Unlock()
		return fmt.Errorf("failed to stop sandbox: %w", err)
	}

	delete(w.runningSandboxes, containerID)
	w.mu.Unlock()
	return nil
}

// RemoveSandbox stops and removes the specified running sandbox.
// if the sandbox is not running it will return an error.
func (w *WorkerService) RemoveSandbox(ctx context.Context, sandboxID string) error {
	w.mu.Lock()
	if _, deleting := w.deletingSandboxes[sandboxID]; deleting {
		w.mu.Unlock()
		return fmt.Errorf("sandbox %s is already being deleted", sandboxID)
	}
	w.deletingSandboxes[sandboxID] = struct{}{}
	client := w.runningSandboxes[sandboxID]

	defer func() {
		w.mu.Lock()
		delete(w.runningSandboxes, sandboxID)
		delete(w.deletingSandboxes, sandboxID)
		w.mu.Unlock()
	}()

	if client == nil {
		delete(w.deletingSandboxes, sandboxID)
		w.mu.Unlock()
		return fmt.Errorf("sandbox %s is not running", sandboxID)
	}
	w.mu.Unlock()

	// Stop the Firecracker VM. A stop failure shouldn't block cleanup below —
	// leaving the directory (and a possibly still-running process) behind
	// forever is worse than a best-effort cleanup that reports the error.
	// ReapOrphans catches anything this misses on the next worker restart.
	stopErr := client.Stop(ctx)
	if stopErr != nil {
		log.Printf("remove sandbox %s: stop failed, cleaning up anyway: %v", sandboxID, stopErr)
	}

	if err := os.RemoveAll(client.Dir); err != nil && !os.IsNotExist(err) {
		if stopErr != nil {
			return fmt.Errorf("stop sandbox %s: %v; remove sandbox dir: %w", sandboxID, stopErr, err)
		}
		return fmt.Errorf("remove sandbox dir: %w", err)
	}

	if stopErr != nil {
		return fmt.Errorf("failed to stop sandbox %s (dir cleaned up): %w", sandboxID, stopErr)
	}

	return nil
}

func (w *WorkerService) ListRunningSandboxes(ctx context.Context) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	sandboxes := make([]string, 0, len(w.runningSandboxes))
	for id := range w.runningSandboxes {
		sandboxes = append(sandboxes, id)
	}
	return sandboxes
}
