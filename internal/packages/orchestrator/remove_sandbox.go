package orchestrator

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

// removeSandboxOnWorker asks workerID to remove sandboxID and returns nil only
// once the worker confirms the sandbox is gone. A worker that no longer tracks
// the sandbox (gRPC NotFound) counts as confirmation; every other failure —
// unknown worker, unreachable worker, remove error — is returned so the caller
// can keep its state and let River retry.
//
// The whole call is bounded by removeSandboxCallTimeout: a worker that drops
// packets instead of refusing them would otherwise hold the job until River's
// own timeout. A removal that timed out is not confirmed, and is retried.
func removeSandboxOnWorker(ctx context.Context, workerRepo workerpkg.Repository, workerID, sandboxID string) error {
	ctx, cancel := context.WithTimeout(ctx, removeSandboxCallTimeout)
	defer cancel()

	worker, err := workerRepo.Get(ctx, workerID)
	if err != nil {
		return fmt.Errorf("get worker %s: %w", workerID, err)
	}

	client, err := NewClient(ctx, normalizeWorkerAddress(worker.Address, int32(worker.Port)))
	if err != nil {
		return fmt.Errorf("connect worker %s: %w", workerID, err)
	}
	defer client.Close()

	_, err = client.RemoveSandbox(ctx, &workerv1.RemoveSandboxRequest{ContainerId: sandboxID})
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("remove sandbox %s on worker %s: %w", sandboxID, workerID, err)
	}
	return nil
}
