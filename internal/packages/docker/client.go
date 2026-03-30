package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

type Client struct {
	cli *client.Client
}

type ClientInterface interface {
	PullImage(ctx context.Context, img string) error
	CreateContainer(ctx context.Context, img string, hostPort string, id string) (string, error)
	StartContainer(ctx context.Context, containerID string) error
	StopContainer(ctx context.Context, containerID string) error
	RemoveContainer(ctx context.Context, containerID string) error
	InspectImage(ctx context.Context, img string) error
}

func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli}, nil
}

func (c *Client) PullImage(ctx context.Context, img string) error {
	reader, err := c.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

func (c *Client) CreateContainer(ctx context.Context, img string, hostPort string, id string) (string, error) {
	config := &container.Config{
		Image: img,
		ExposedPorts: nat.PortSet{
			"3000/tcp": struct{}{},
		},
		Labels: map[string]string{
			"id": id,
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			"3000/tcp": []nat.PortBinding{
				{
					HostIP:   "0.0.0.0",
					HostPort: hostPort,
				},
			},
		},
	}

	resp, err := c.cli.ContainerCreate(ctx, config, hostConfig, nil, nil, "")
	if err != nil {
		return "", err
	}

	return resp.ID, nil
}

func (c *Client) StartContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (c *Client) StopContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerStop(ctx, containerID, container.StopOptions{})
}

func (c *Client) RemoveContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// InspectImage checks if the specified image exists locally by inspecting it. If the image is not found, it returns an error.
func (c *Client) InspectImage(ctx context.Context, img string) error {
	_, _, err := c.cli.ImageInspectWithRaw(ctx, img)
	return err
}

func getContainerByLabel(ctx context.Context, cli *client.Client, labelKey string, labelValue string) (string, error) {
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", labelKey+"="+labelValue)

	containers, err := cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return "", err
	}

	if len(containers) == 0 {
		return "", nil // No container found with the specified label
	}

	return containers[0].ID, nil // Return the ID of the first matching container
}
