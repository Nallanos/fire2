# Sandbox flow helpers

-include .env
export

.PHONY: sandbox-up sandbox-migrate sandbox-api sandbox-worker sandbox-register-worker sandbox-smoke sandbox-flow sandbox-check-env sandbox-start sandbox-seed

SANDBOX_WORKERS ?= 2
SANDBOX_WORKER_PORT_BASE ?= 50051
SANDBOX_LOG_DIR ?= .sandbox

sandbox-check-env:
	@test -n "$$DATABASE_URL" || (echo "DATABASE_URL is required"; exit 1)

sandbox-up:
	$(COMPOSE) up -d postgresql

sandbox-migrate: sandbox-check-env
	$(DBMATE) --migrations-dir internal/db/migrations up

sandbox-api: sandbox-check-env
	$(GO) run ./cmd/api

sandbox-worker: sandbox-check-env
	$(GO) run ./cmd/worker

sandbox-register-worker: sandbox-check-env
	$(GO) run ./cmd/create_worker --port $${WORKER_PORT:-50051} --cpu-budget $${WORKER_CPU_BUDGET:-4} --mem-budget $${WORKER_MEM_BUDGET:-8192}

sandbox-start: sandbox-check-env
	@mkdir -p $(SANDBOX_LOG_DIR)
	@api_port=$${PORT:-8081}; \
	grpc_port=$${ORCHESTRATOR_GRPC_PORT:-7001}; \
	grpc_addr=$${ORCHESTRATOR_GRPC_ADDR:-127.0.0.1:$$grpc_port}; \
	count=$${SANDBOX_WORKERS:-2}; \
	base_port=$${SANDBOX_WORKER_PORT_BASE:-50051}; \
	echo "starting api on $$api_port"; \
	PORT=$$api_port ORCHESTRATOR_GRPC_PORT=$$grpc_port DATABASE_URL=$$DATABASE_URL \
		nohup $(GO) run ./cmd/api > $(SANDBOX_LOG_DIR)/api.log 2>&1 & \
	for i in $$(seq 1 30); do \
		if curl -sS http://localhost:$$api_port/health >/dev/null 2>&1; then break; fi; \
		sleep 0.5; \
	done; \
	for i in $$(seq 0 $$(($$count - 1))); do \
		port=$$(($$base_port + $$i)); \
		echo "starting worker on $$port"; \
		WORKER_PORT=$$port ORCHESTRATOR_GRPC_ADDR=$$grpc_addr DATABASE_URL=$$DATABASE_URL \
			nohup $(GO) run ./cmd/worker > $(SANDBOX_LOG_DIR)/worker-$$port.log 2>&1 & \
		$(GO) run ./cmd/create_worker --port $$port --cpu-budget $${WORKER_CPU_BUDGET:-4} --mem-budget $${WORKER_MEM_BUDGET:-8192} --db-url $$DATABASE_URL > $(SANDBOX_LOG_DIR)/worker-$$port-register.log 2>&1; \
	done
	@echo "logs in $(SANDBOX_LOG_DIR)/"

sandbox-smoke:
	curl -sS -X POST http://localhost:$${PORT:-8081}/api/sandboxes \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","ttl":3600}' | cat
	curl -sS http://localhost:$${PORT:-8081}/api/sandboxes | cat

sandbox-seed:
	curl -sS -X POST http://localhost:$${PORT:-8081}/api/sandboxes \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","image":"node:20-alpine","ttl":3600}' | cat
	curl -sS -X POST http://localhost:$${PORT:-8081}/api/sandboxes \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"python","image":"python:3.12-alpine","ttl":3600}' | cat
	curl -sS -X POST http://localhost:$${PORT:-8081}/api/sandboxes \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"go","image":"golang:1.23-alpine","ttl":3600}' | cat
	curl -sS http://localhost:$${PORT:-8081}/api/sandboxes | cat

sandbox-flow:
	@echo "Run these in order:"
	@echo "  1) make sandbox-up"
	@echo "  2) make sandbox-migrate"
	@echo "  3) make sandbox-start"
	@echo "  4) make sandbox-seed"
