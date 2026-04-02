package orchestrator

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	"github/nallanos/fire2/internal/db"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultWorkerGRPCPort = 50051

type Client struct {
	conn   *grpc.ClientConn
	worker workerv1.WorkerServiceClient
}

func NewClient(ctx context.Context, address string) (*Client, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}

	return &Client{conn: conn, worker: workerv1.NewWorkerServiceClient(conn)}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) CreateSandbox(ctx context.Context, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	return c.worker.CreateSandbox(ctx, req)
}

func (c *Client) StopSandbox(ctx context.Context, req *workerv1.StopSandboxRequest) (*workerv1.StopSandboxResponse, error) {
	return c.worker.StopSandbox(ctx, req)
}

func (c *Client) RemoveSandbox(ctx context.Context, req *workerv1.RemoveSandboxRequest) (*workerv1.RemoveSandboxResponse, error) {
	return c.worker.RemoveSandbox(ctx, req)
}

func (c *Client) GetWorkerInfo(ctx context.Context, req *workerv1.GetWorkerInfoRequest) (*workerv1.GetWorkerInfoResponse, error) {
	return c.worker.GetWorkerInfo(ctx, req)
}

func CreateSandboxOnLeastUsedWorker(ctx context.Context, workers []db.Worker, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	scheduler := NewScheduler()
	candidates := make([]WorkerCandidate, 0, len(workers))

	for _, worker := range workers {
		address := normalizeWorkerAddress(worker.Address, worker.Port)
		if address == "" {
			continue
		}

		client, err := NewClient(ctx, address)
		if err != nil {
			continue
		}

		info, infoErr := client.GetWorkerInfo(ctx, &workerv1.GetWorkerInfoRequest{})
		_ = client.Close()
		if infoErr != nil {
			continue
		}

		candidates = append(candidates, WorkerCandidate{
			Worker: worker,
			Info:   info,
		})
	}

	selected, err := scheduler.ChooseLeastUsedWorker(candidates)
	if err != nil {
		return nil, err
	}

	selectedAddress := normalizeWorkerAddress(selected.Worker.Address, selected.Worker.Port)
	client, err := NewClient(ctx, selectedAddress)
	if err != nil {
		return nil, fmt.Errorf("connect selected worker %q: %w", selected.Worker.ID, err)
	}
	defer client.Close()

	return client.CreateSandbox(ctx, req)
}

func normalizeWorkerAddress(address string, port int32) string {
	address = strings.TrimSpace(address)
	if address == "" {
		address = "127.0.0.1"
	}
	if port <= 0 {
		port = defaultWorkerGRPCPort
	}

	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}

	return net.JoinHostPort(address, strconv.Itoa(int(port)))
}
