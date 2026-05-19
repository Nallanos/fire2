package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github/nallanos/fire2/internal/app"
	dbpkg "github/nallanos/fire2/internal/db"
	"github/nallanos/fire2/internal/packages/orchestrator"
)

func main() {
	cfg := app.ConfigFromEnv()

	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	defer sqlDB.Close()

	if err != nil {
		log.Fatal(err)
	}

	queries := dbpkg.New(sqlDB)
	a := app.New(cfg, sqlDB)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           a.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("api listening on :%s", cfg.Port)
		errCh <- srv.ListenAndServe()
	}()

	grpcErrCh := make(chan error, 1)
	go func() {
		grpcAddr := ":" + cfg.OrchestratorGRPCPort
		log.Printf("orchestrator gRPC listening on %s", grpcAddr)
		grpcErrCh <- orchestrator.ServeEventGRPC(grpcAddr, orchestrator.NewEventGRPCServer(queries))
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("shutdown signal: %s", sig)
	case err := <-errCh:
		log.Printf("server error: %v", err)
	case err := <-grpcErrCh:
		log.Printf("grpc server error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)
}
