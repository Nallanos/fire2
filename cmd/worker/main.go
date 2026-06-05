package main

import (
	"context"
	"log"
	"os"

	"github/nallanos/fire2/internal/packages/docker"
	"github/nallanos/fire2/internal/packages/orchestrator"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

const defaultWorkerPort = "50051"

func main() {
	workerPort := os.Getenv("WORKER_PORT")
	if workerPort == "" {
		workerPort = defaultWorkerPort
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
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	reporter := workerpkg.NewEventReporter(dockerClient, eventClient.Client(), workerID)
	go reporter.Run(context.Background())

	log.Printf("worker gRPC listening on :%s", workerPort)
	if err := workerpkg.ServeGRPC(":"+workerPort, workerGRPCServer); err != nil {
		log.Fatal(err)
	}
}
