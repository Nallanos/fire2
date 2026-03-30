package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/google/uuid"

	"github/nallanos/fire2/internal/db"
)

func main() {
	var (
		id        string
		host      string
		port      int
		cpuBudget int
		memBudget int
		status    string
		dbURL     string
	)

	flag.StringVar(&id, "id", "", "worker id (optional, auto-generated if empty)")
	flag.StringVar(&host, "host", "127.0.0.1", "worker host/address (default localhost)")
	flag.IntVar(&port, "port", 0, "worker gRPC port (required)")
	flag.IntVar(&cpuBudget, "cpu-budget", 0, "theoretical CPU budget in cores (required)")
	flag.IntVar(&memBudget, "mem-budget", 0, "theoretical memory budget in MB (required)")
	flag.StringVar(&status, "status", "active", "worker status")
	flag.StringVar(&dbURL, "db-url", "", "database URL (defaults to DATABASE_URL env var)")
	flag.Parse()

	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		log.Fatal("missing database URL: use --db-url or set DATABASE_URL")
	}
	if port <= 0 {
		log.Fatal("--port must be > 0")
	}
	if cpuBudget <= 0 {
		log.Fatal("--cpu-budget must be > 0")
	}
	if memBudget <= 0 {
		log.Fatal("--mem-budget must be > 0")
	}
	if id == "" {
		id = uuid.NewString()
	}

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	queries := db.New(sqlDB)

	_, err = queries.CreateWorker(ctx, db.CreateWorkerParams{
		ID:        id,
		Status:    status,
		Address:   host,
		Capacity:  int32(cpuBudget),
		Port:      int32(port),
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		_, updateErr := queries.UpdateWorker(ctx, db.UpdateWorkerParams{
			ID:       id,
			Status:   status,
			Address:  host,
			Capacity: int32(cpuBudget),
			Port:     int32(port),
		})
		if updateErr != nil {
			log.Fatalf("create worker failed: %v; update fallback failed: %v", err, updateErr)
		}
	}

	fmt.Printf("worker ready: id=%s host=%s port=%d cpu_budget=%d mem_budget=%d\n", id, host, port, cpuBudget, memBudget)
}
