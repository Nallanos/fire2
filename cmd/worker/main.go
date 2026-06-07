package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github/nallanos/fire2/internal/packages/docker"
	"github/nallanos/fire2/internal/packages/orchestrator"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

func main() {
	// WORKER_PORT unset or "0" binds an OS-assigned ephemeral port; the worker
	// discovers the real port from the listener and reports it via heartbeat.
	// A non-zero value pins a specific port.
	workerPort := os.Getenv("WORKER_PORT")
	if workerPort == "" {
		workerPort = "0"
	}

	orchestratorAddr := os.Getenv("ORCHESTRATOR_GRPC_ADDR")
	if orchestratorAddr == "" {
		orchestratorAddr = "127.0.0.1:7001"
	}

	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID, _ = os.Hostname()
	}
	advertisedHost := os.Getenv("WORKER_ADVERTISED_HOST")

	cpuBudget, _ := strconv.Atoi(os.Getenv("WORKER_CPU_BUDGET"))
	memBudget, _ := strconv.Atoi(os.Getenv("WORKER_MEM_BUDGET"))

	ctx := context.Background()

	dockerClient, err := docker.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	eventClient, err := orchestrator.NewEventClient(ctx, orchestratorAddr)
	if err != nil {
		log.Fatalf("orchestrator event client init failed: %v", err)
	}
	defer eventClient.Close()

	workerService := workerpkg.NewWorkerService(dockerClient, eventClient.Client())
	workerService.SetWorkerIdentity(workerID, advertisedHost)
	workerService.SetWorkerBudget(cpuBudget, memBudget)
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	reporter := workerpkg.NewEventReporter(dockerClient, eventClient.Client(), workerID)
	go reporter.Run(context.Background())

	if err := workerpkg.ServeGRPC(":"+workerPort, workerGRPCServer); err != nil {
		log.Fatal(err)
	}
}
