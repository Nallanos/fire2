package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"strconv"

	"github/nallanos/fire2/internal/logging"
	"github/nallanos/fire2/internal/packages/orchestrator"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	"github/nallanos/fire2/internal/packages/sandboxmgr"
	tailscalepkg "github/nallanos/fire2/internal/packages/tailscale"
	workerpkg "github/nallanos/fire2/internal/packages/worker"
)

func main() {
	logging.SetupLogger("fire2-worker")

	reapAll := flag.Bool("reap-all", false,
		"stop every sandbox on this host (VMs, shims, volumes, directories) and exit; "+
			"for maintenance, with the worker service stopped")
	flag.Parse()

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
	slog.Info("orchestrator gRPC address", "address", orchestratorAddr)

	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID, _ = os.Hostname()
	}
	slog.SetDefault(slog.With("worker_id", workerID))

	// WORKER_ADVERTISED_HOST/WORKER_GRPC_BIND_HOST are normally left unset —
	// a static IP baked into Ansible vars goes stale the moment a host is
	// rebuilt or moved to a new provider (see the Oracle migration
	// incident). Auto-discover the tailnet IP instead; the env vars remain
	// as an explicit override for the rare case that needs one.
	advertisedHost := os.Getenv("WORKER_ADVERTISED_HOST")
	if advertisedHost == "" {
		if ip, err := tailscalepkg.IPv4(); err == nil {
			advertisedHost = ip
			slog.Info("auto-discovered tailscale address", "address", ip)
		} else {
			slog.Warn("tailscale address auto-discovery failed, falling back", "err", err)
		}
	}

	// Restricts the gRPC listener to a single interface (e.g. the
	// auto-discovered Tailscale IP above) instead of all interfaces. Empty
	// means bind everywhere — the local-dev default. gRPC uses insecure
	// credentials, so in a multi-host deployment this must be a private
	// address, never public.
	grpcBindHost := os.Getenv("WORKER_GRPC_BIND_HOST")
	if grpcBindHost == "" {
		grpcBindHost = advertisedHost
	}

	cpuBudget, _ := strconv.Atoi(os.Getenv("WORKER_CPU_BUDGET"))
	memBudget, _ := strconv.Atoi(os.Getenv("WORKER_MEM_BUDGET"))

	ctx := context.Background()

	sandboxCfg := sandboxmgr.ConfigFromEnv()

	// One worker per host: this process owns every sandbox in the base dir
	// and reaps whatever it does not track, so a second one would reap the
	// first one's sandboxes. Lock before reaping anything. The lock lives
	// as long as the file stays open, i.e. the whole process.
	baseDirLock, err := sandboxmgr.LockBaseDir(sandboxCfg.BaseDir)
	if err != nil {
		slog.Error("worker startup failed", "err", err)
		os.Exit(1)
	}
	defer baseDirLock.Close()

	if *reapAll {
		if err := runtimeClient.ReapOrphans(sandboxCfg.BaseDir, sandboxCfg.RemoveVolume); err != nil {
			slog.Error("reap all sandboxes failed", "err", err)
			os.Exit(1)
		}
		slog.Info("every sandbox reaped")
		return
	}

	// VMs outlive the worker: each runs under its own fire2-shim. Take back
	// the ones a previous crash/restart left running, and reap what cannot
	// be taken back. Done before the first heartbeat, so the orchestrator
	// sees the recovered sandboxes in this incarnation's first inventory
	// instead of failing them as lost.
	sandboxes := sandboxmgr.New(ctx, sandboxCfg)
	recovered, err := sandboxes.Recover()
	if err != nil {
		slog.Error("recover sandboxes failed", "err", err)
	}
	slog.Info("recovered sandboxes", "count", recovered)

	eventClient, err := orchestrator.NewEventClient(ctx, orchestratorAddr)
	if err != nil {
		slog.Error("orchestrator event client init failed", "err", err)
		os.Exit(1)
	}
	defer eventClient.Close()

	go sandboxes.Run(ctx, 0)

	workerService := workerpkg.NewWorkerService(eventClient.Client(), sandboxes)
	workerService.SetWorkerIdentity(workerID, advertisedHost)
	workerService.SetWorkerBudget(cpuBudget, memBudget)
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	reporter := workerpkg.NewEventReporter(eventClient.Client(), workerID, workerService)
	go reporter.Run(ctx)

	if err := workerpkg.ServeGRPC(grpcBindHost+":"+workerPort, workerGRPCServer); err != nil {
		slog.Error("worker gRPC server failed", "err", err)
		if errors.Is(err, workerpkg.ErrHeartbeatRefused) {
			// A restart would only be refused again: the unit must not
			// restart it (RestartPreventExitStatus). The VMs live on.
			os.Exit(workerpkg.ExitHeartbeatRefused)
		}
		os.Exit(1)
	}
}
