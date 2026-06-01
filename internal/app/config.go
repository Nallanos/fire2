package app

import (
	"os"
	"time"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	OrchestratorGRPCPort string
	// SandboxWaitTimeout overrides the 45-second default in the createSandbox
	// handler. Zero means use the default. Intended for tests.
	SandboxWaitTimeout time.Duration
}

func ConfigFromEnv() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgresql://temporal:temporal@localhost/temporal"
	}

	orchestratorGRPCPort := os.Getenv("ORCHESTRATOR_GRPC_PORT")
	if orchestratorGRPCPort == "" {
		orchestratorGRPCPort = "7001"
	}

	return Config{Port: port, DatabaseURL: databaseURL, OrchestratorGRPCPort: orchestratorGRPCPort}
}
