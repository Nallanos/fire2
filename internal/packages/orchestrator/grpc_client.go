package orchestrator

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	workerv1 "github/nallanos/fire2/gen/worker/v1"
	workerpkg "github/nallanos/fire2/internal/packages/worker"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

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

func CreateSandboxOnLeastUsedWorker(ctx context.Context, workers []workerpkg.Worker, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	return CreateSandboxOnLeastUsedWorkerWithScheduler(ctx, NewScheduler(), workers, req)
}

func CreateSandboxOnLeastUsedWorkerWithScheduler(ctx context.Context, scheduler *Scheduler, workers []workerpkg.Worker, req *workerv1.CreateSandboxRequest) (*workerv1.CreateSandboxResponse, error) {
	if scheduler == nil {
		scheduler = NewScheduler()
	}

	candidates := make([]WorkerCandidate, 0, len(workers))

	for _, worker := range workers {
		address := normalizeWorkerAddress(worker.Address, int32(worker.Port))
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

	selectedAddress := normalizeWorkerAddress(selected.Worker.Address, int32(selected.Worker.Port))
	client, err := NewClient(ctx, selectedAddress)
	if err != nil {
		return nil, fmt.Errorf("connect selected worker %q: %w", selected.Worker.ID, err)
	}
	defer client.Close()

	return client.CreateSandbox(ctx, req)
}

// DestroySandboxOnWorker stops and removes the container at sandboxID on the
// worker at workerAddress. Errors are logged but not returned — this is
// best-effort cleanup for the abandoned-sandbox case.
func DestroySandboxOnWorker(ctx context.Context, workerAddress, sandboxID string) {
	client, err := NewClient(ctx, workerAddress)
	if err != nil {
		log.Printf("destroy sandbox: connect %q: %v", workerAddress, err)
		return
	}
	defer client.Close()
	if _, err := client.StopSandbox(ctx, &workerv1.StopSandboxRequest{ContainerId: sandboxID}); err != nil {
		log.Printf("destroy sandbox: stop %s on %s: %v", sandboxID, workerAddress, err)
	}
	if _, err := client.RemoveSandbox(ctx, &workerv1.RemoveSandboxRequest{ContainerId: sandboxID}); err != nil {
		log.Printf("destroy sandbox: remove %s on %s: %v", sandboxID, workerAddress, err)
	}
}

// normalizeWorkerAddress builds a dial target from a worker's stored address
// and port. Workers self-report an ephemeral port via heartbeat, so a port of
// 0 means "not reported yet" — there is no port to guess, so we return "" and
// the caller skips that worker.
func normalizeWorkerAddress(address string, port int32) string {
	address = strings.TrimSpace(address)
	if address == "" {
		address = "127.0.0.1"
	}

	// An address that already embeds a port is a complete dial target.
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}

	if port <= 0 {
		return ""
	}

	return net.JoinHostPort(address, strconv.Itoa(int(port)))
}
