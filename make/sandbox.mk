# Sandbox flow helpers

-include .env
export

.PHONY: sandbox-up sandbox-migrate sandbox-api sandbox-worker sandbox-smoke sandbox-flow sandbox-check-env sandbox-start sandbox-seed sandbox-token

SANDBOX_WORKERS ?= 2
SANDBOX_WORKER_PORT_BASE ?= 50051
SANDBOX_LOG_DIR ?= .sandbox

# API_HOST is where sandbox-smoke/seed/list/ansible-smoke send their curls.
# Defaults to the real prod orchestrator (MainDev) — override for local
# testing, e.g. `make sandbox-list API_HOST=localhost:8081`. Each host gets
# its own cached token file so a stale prod token never gets sent to local
# (or vice versa) just because a file happened to already exist.
PROD_API_HOST ?= 100.78.175.35:8081
API_HOST ?= $(PROD_API_HOST)
ifeq ($(API_HOST),$(PROD_API_HOST))
SANDBOX_TOKEN_FILE ?= $(SANDBOX_LOG_DIR)/token-prod
else
SANDBOX_TOKEN_FILE ?= $(SANDBOX_LOG_DIR)/token-local
endif

sandbox-check-env:
	@test -n "$$DATABASE_URL" || (echo "DATABASE_URL is required"; exit 1)

sandbox-up:
	$(COMPOSE) up -d postgresql

sandbox-migrate: sandbox-check-env
	$(DBMATE) --migrations-dir internal/db/migrations up

sandbox-api: sandbox-check-env
	$(GO) run ./cmd/api

sandbox-worker:
	$(GO) run ./cmd/worker

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
		echo "starting worker"; \
		nohup $(GO) run ./cmd/worker > $(SANDBOX_LOG_DIR)/worker.log 2>&1 & \
	done
	@echo "logs in $(SANDBOX_LOG_DIR)/"

# sandbox-token issues (once, then caches) a non-expiring bearer token for
# the sandbox-* curl targets below. Never used by the public API itself —
# real logins always expire normally. Prod (the default) uses PROD_TOKEN
# from .env, generated once via cmd/devtoken run directly on MainDev against
# its own DATABASE_URL (see .env comment) — this machine has no DB access to
# mint prod sessions itself. Local (API_HOST=localhost:8081) generates its
# own token here via cmd/devtoken, same as before.
sandbox-token:
	@mkdir -p $(SANDBOX_LOG_DIR)
	@if [ "$(API_HOST)" = "$(PROD_API_HOST)" ]; then \
		test -n "$$PROD_TOKEN" || (echo "PROD_TOKEN is required in .env to call the prod API (API_HOST=$(API_HOST))"; exit 1); \
		echo "$$PROD_TOKEN" > $(SANDBOX_TOKEN_FILE); \
	else \
		test -n "$$DATABASE_URL" || (echo "DATABASE_URL is required"; exit 1); \
		test -s $(SANDBOX_TOKEN_FILE) || $(GO) run ./cmd/devtoken > $(SANDBOX_TOKEN_FILE); \
	fi

sandbox-smoke: sandbox-token
	@echo "hitting API at $(API_HOST)"
	curl -sS -X POST http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","ttl":3600}' | cat
	curl -sS http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" | cat

sandbox-seed: sandbox-token
	@echo "hitting API at $(API_HOST)"
	curl -sS -X POST http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","image":"node:20-alpine","ttl":3600}' | cat
	curl -sS -X POST http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"python","image":"python:3.12-alpine","ttl":3600}' | cat
	curl -sS -X POST http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"go","image":"golang:1.23-alpine","ttl":3600}' | cat
	curl -sS http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" | cat

sandbox-flow:
	@echo "Run these in order:"
	@echo "  1) make sandbox-up"
	@echo "  2) make sandbox-migrate"
	@echo "  3) make sandbox-start"
	@echo "  4) make sandbox-seed"

sandbox-list: sandbox-token
	@echo "hitting API at $(API_HOST)"
	curl -sS http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" | cat