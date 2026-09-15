package runtime

import (
	"context"
	"fmt"
	"time"

	orchestratorv1 "github/nallanos/fire2/gen/orchestrator/v1"

	"google.golang.org/protobuf/types/known/timestamppb"
)

func (c *Client) GetMetrics(ctx context.Context) (*orchestratorv1.SandboxEvent, error) {
	if c.machine == nil {
		return nil, fmt.Errorf("machine not created")
	}

	instanceInfo, err := c.machine.DescribeInstanceInfo(ctx)
	if err != nil {
		return nil, err
	}

	return &orchestratorv1.SandboxEvent{
		Id:         *instanceInfo.ID,
		SandboxId:  c.sandboxId,
		State:      *instanceInfo.State,
		OccurredAt: timestamppb.New(time.Now()),
	}, nil
}
