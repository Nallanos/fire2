package app

import (
	"os"
	"time"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	OrchestratorGRPCPort string
	// OrchestratorGRPCBindHost restricts the gRPC listener to a single
	// interface (e.g. a Tailscale IP) instead of all interfaces. Empty means
	// bind everywhere — the local-dev default. gRPC uses insecure
	// credentials, so in a multi-host deployment this must be a private
	// address, never a public one.
	OrchestratorGRPCBindHost string
	// SandboxWaitTimeout overrides the 45-second default in the createSandbox
	// handler. Zero means use the default. Intended for tests.
	SandboxWaitTimeout time.Duration
	// HeartbeatTimeout is how long a worker can go without heartbeating before
	// the reaper marks it inactive. Defaults to defaultHeartbeatTimeout (15s).
	HeartbeatTimeout time.Duration
	// ReaperInterval is how often the worker-reaper periodic job runs.
	ReaperInterval time.Duration
}

const (
	defaultHeartbeatTimeout = 15 * time.Second
	defaultReaperInterval   = 10 * time.Second
)

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

	return Config{
		Port:                     port,
		DatabaseURL:              databaseURL,
		OrchestratorGRPCPort:     orchestratorGRPCPort,
		OrchestratorGRPCBindHost: os.Getenv("ORCHESTRATOR_GRPC_BIND_HOST"),
		HeartbeatTimeout:         durationFromEnv("HEARTBEAT_TIMEOUT", defaultHeartbeatTimeout),
		ReaperInterval:           durationFromEnv("REAPER_INTERVAL", defaultReaperInterval),
	}
}

// durationFromEnv parses a Go duration string (e.g. "15s") from the named env
// var, falling back to def when unset or unparseable.
func durationFromEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
