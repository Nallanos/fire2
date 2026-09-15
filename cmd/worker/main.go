package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github/nallanos/fire2/internal/packages/orchestrator"
	runtimeClient "github/nallanos/fire2/internal/packages/runtime"
	tailscalepkg "github/nallanos/fire2/internal/packages/tailscale"
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
	log.Printf("orchestrator gRPC address: %s", orchestratorAddr)

	workerID := os.Getenv("WORKER_ID")
	if workerID == "" {
		workerID, _ = os.Hostname()
	}

	// WORKER_ADVERTISED_HOST/WORKER_GRPC_BIND_HOST are normally left unset —
	// a static IP baked into Ansible vars goes stale the moment a host is
	// rebuilt or moved to a new provider (see the Oracle migration
	// incident). Auto-discover the tailnet IP instead; the env vars remain
	// as an explicit override for the rare case that needs one.
	advertisedHost := os.Getenv("WORKER_ADVERTISED_HOST")
	if advertisedHost == "" {
		if ip, err := tailscalepkg.IPv4(); err == nil {
			advertisedHost = ip
			log.Printf("auto-discovered tailscale address: %s", ip)
		} else {
			log.Printf("tailscale address auto-discovery failed, falling back: %v", err)
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

	// Firecracker keeps no state of its own across restarts — kill any VM
	// left running by a previous crash/redeploy of this worker and clean up
	// its socket/directory. The orchestrator's retry policy re-creates the
	// sandbox; this just guarantees a clean slate for that retry.
	if err := runtimeClient.ReapOrphans(runtimeClient.SandboxBaseDir()); err != nil {
		log.Printf("reap orphaned sandboxes: %v", err)
	}

	eventClient, err := orchestrator.NewEventClient(ctx, orchestratorAddr)
	if err != nil {
		log.Fatalf("orchestrator event client init failed: %v", err)
	}
	defer eventClient.Close()

	workerService := workerpkg.NewWorkerService(eventClient.Client())
	workerService.SetWorkerIdentity(workerID, advertisedHost)
	workerService.SetWorkerBudget(cpuBudget, memBudget)
	workerGRPCServer := workerpkg.NewWorkerGRPCServer(workerService)

	reporter := workerpkg.NewEventReporter(eventClient.Client(), workerID, workerService)
	go reporter.Run(context.Background())

	if err := workerpkg.ServeGRPC(grpcBindHost+":"+workerPort, workerGRPCServer); err != nil {
		log.Fatal(err)
	}
}
