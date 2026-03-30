package main

import (
	"database/sql"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/docker"
	workerpkg "github/nallanos/fire2/internal/worker"
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
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	log.Printf("worker gRPC listening on :%s", workerPort)
	if err := workerpkg.ServeGRPC(":"+workerPort, workerGRPCServer); err != nil {
		log.Fatal(err)
	}
}
