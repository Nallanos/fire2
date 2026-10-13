package runtime

import (
	"context"
	"fmt"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"
)

// State samples the VM's instance info and maps firecracker-go-sdk's
// InstanceInfo.State ("Not started" | "Running" | "Paused") onto the closed
// SandboxState vocabulary. An error means the API could not be queried; the
// caller decides whether that is UNKNOWN or DEAD (see Wait).
func (c *Client) State(ctx context.Context) (orchestratorv1.SandboxState, error) {
	if c.machine == nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("machine not created")
	}

	info, err := c.machine.DescribeInstanceInfo(ctx)
	if err != nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, err
	}
	if info.State == nil {
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("instance info has no state")
	}

	switch *info.State {
	case "Running":
		return orchestratorv1.SandboxState_SANDBOX_STATE_RUNNING, nil
	case "Paused":
		return orchestratorv1.SandboxState_SANDBOX_STATE_PAUSED, nil
	case "Not started":
		return orchestratorv1.SandboxState_SANDBOX_STATE_STARTING, nil
	default:
		return orchestratorv1.SandboxState_SANDBOX_STATE_UNSPECIFIED, fmt.Errorf("unexpected instance state %q", *info.State)
	}
}
