package main

import (
	"context"
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/docker"
	"github/nallanos/fire2/internal/packages/orchestrator"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

const defaultWorkerPort = "50051"

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgresql://temporal:temporal@localhost/temporal"
	}

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

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()

	dockerClient, err := docker.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	queries := db.New(sqlDB)
	workerService := workerpkg.NewWorkerService(dockerClient, queries)
	workerService.SetWorkerIdentity(workerID, advertisedHost)
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	if eventClient, err := orchestrator.NewEventClient(context.Background(), orchestratorAddr); err != nil {
		log.Printf("event client init failed: %v", err)
	} else {
		defer eventClient.Close()
		reporter := workerpkg.NewEventReporter(dockerClient, eventClient.Client(), workerID)
		go reporter.Run(context.Background())
	}

	log.Printf("worker gRPC listening on :%s", workerPort)
	if err := workerpkg.ServeGRPC(":"+workerPort, workerGRPCServer); err != nil {
		log.Fatal(err)
	}
}
