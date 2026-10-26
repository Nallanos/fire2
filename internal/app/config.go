package app

import (
	"log/slog"
	"os"
	"time"

	tailscalepkg "github/nallanos/fire2/internal/packages/tailscale"
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
	// SandboxLivenessTimeout is how long a hosting worker may keep heartbeating
	// without listing a sandbox in its inventory before the sandbox is failed
	// and cleaned up. Measured on the worker's heartbeats, not the
	// orchestrator's clock. Defaults to 15s (three 5s worker heartbeats).
	SandboxLivenessTimeout time.Duration
	// SandboxWorkerLostTimeout is how long a worker must stay inactive before
	// its sandboxes are failed: a machine gone for good, as opposed to one the
	// orchestrator lost sight of. It must exceed the longest orchestrator or
	// database outage, or network cut between the two, that should cost no
	// sandbox: a worker whose heartbeats cannot get through is inactive for as
	// long as the outage lasts, and its sandboxes are failed once it passes.
	// Defaults to 2m.
	SandboxWorkerLostTimeout time.Duration
	// SandboxReaperInterval is how often the TTL/liveness sandbox reaper runs.
	SandboxReaperInterval time.Duration
	// SandboxCreateTimeout is how long a sandbox may stay pending or scheduling
	// before the reaper fails it. A create job that gave up without leaving a
	// trace (discarded, orchestrator lost) would otherwise hold its row until
	// the sandbox's TTL. It must exceed the longest a healthy sandbox stays in
	// those two statuses: the scheduling step of its job, 5 attempts of at most
	// 5s of GetWorkerInfo calls with 2+4+8+16s of backoff between them (about a
	// minute), plus the time its job waits in the queue under load. The
	// CreateSandbox call (45s per attempt) comes after the sandbox is assigned
	// and does not count: from then on liveness watches it. Defaults to 5m.
	SandboxCreateTimeout time.Duration
}

const (
	defaultHeartbeatTimeout = 15 * time.Second
	defaultReaperInterval   = 10 * time.Second

	defaultSandboxLivenessTimeout   = 15 * time.Second
	defaultSandboxWorkerLostTimeout = 2 * time.Minute
	defaultSandboxReaperInterval    = time.Minute

	defaultSandboxCreateTimeout = 5 * time.Minute
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

	// ORCHESTRATOR_GRPC_BIND_HOST is normally left unset — a static IP baked
	// into Ansible vars goes stale the moment a host is rebuilt or moved to
	// a new provider (same fix as cmd/worker). Auto-discover the tailnet IP
	// instead; the env var remains as an explicit override.
	bindHost := os.Getenv("ORCHESTRATOR_GRPC_BIND_HOST")
	if bindHost == "" {
		if ip, err := tailscalepkg.IPv4(); err == nil {
			bindHost = ip
			slog.Info("auto-discovered tailscale address", "address", ip)
		} else {
			slog.Warn("tailscale address auto-discovery failed, falling back", "err", err)
		}
	}

	// Zero would fail the sandboxes of every worker the moment it is inactive,
	// the very outage this timeout rides out. Not a way to turn the rule off.
	workerLost := durationFromEnv("SANDBOX_WORKER_LOST_TIMEOUT", defaultSandboxWorkerLostTimeout)
	if workerLost <= 0 {
		slog.Warn("SANDBOX_WORKER_LOST_TIMEOUT must be positive, using the default", "default", defaultSandboxWorkerLostTimeout)
		workerLost = defaultSandboxWorkerLostTimeout
	}

	return Config{
		Port:                     port,
		DatabaseURL:              databaseURL,
		OrchestratorGRPCPort:     orchestratorGRPCPort,
		OrchestratorGRPCBindHost: bindHost,
		HeartbeatTimeout:         durationFromEnv("HEARTBEAT_TIMEOUT", defaultHeartbeatTimeout),
		ReaperInterval:           durationFromEnv("REAPER_INTERVAL", defaultReaperInterval),
		SandboxLivenessTimeout:   durationFromEnv("SANDBOX_LIVENESS_TIMEOUT", defaultSandboxLivenessTimeout),
		SandboxWorkerLostTimeout: workerLost,
		SandboxReaperInterval:    durationFromEnv("SANDBOX_REAPER_INTERVAL", defaultSandboxReaperInterval),
		SandboxCreateTimeout:     durationFromEnv("SANDBOX_CREATE_TIMEOUT", defaultSandboxCreateTimeout),
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
