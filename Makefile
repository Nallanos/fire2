GO ?= go
COMPOSE ?= docker compose
DBMATE ?= dbmate

include make/sandbox.mk

.PHONY: test

test:
	$(GO) test ./...

# Pinned code generators: the generated files embed these versions in their
# headers, so using other ones rewrites gen/ wholesale. They are installed under
# .tools/ (gitignored) and never taken from the PATH.
# protoc 21.12 reports itself as libprotoc 3.21.12.
PROTOC_VERSION             ?= 21.12
PROTOC_GEN_GO_VERSION      ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
TOOLS_DIR := $(CURDIR)/.tools
PROTOC    := $(TOOLS_DIR)/bin/protoc
PROTOC_OS := $(if $(filter Darwin,$(shell uname -s)),osx,linux)
PROTOC_ARCH := $(if $(filter arm64 aarch64,$(shell uname -m)),aarch_64,x86_64)

.PHONY: proto-tools
proto-tools:
	@mkdir -p $(TOOLS_DIR)/bin
	@test "$$($(PROTOC) --version 2>/dev/null)" = "libprotoc 3.$(PROTOC_VERSION)" || { \
		echo "installing protoc $(PROTOC_VERSION)"; \
		curl -fsSL -o $(TOOLS_DIR)/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-$(PROTOC_OS)-$(PROTOC_ARCH).zip && \
		unzip -o -q $(TOOLS_DIR)/protoc.zip -d $(TOOLS_DIR) 'bin/protoc' 'include/*' && rm $(TOOLS_DIR)/protoc.zip; }
	@GOBIN=$(TOOLS_DIR)/bin $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@GOBIN=$(TOOLS_DIR)/bin $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

.PHONY: proto
proto: proto-tools
	PATH="$(TOOLS_DIR)/bin:$$PATH" $(PROTOC) -I proto \
		--go_out=. --go_opt=module=github/nallanos/fire2 \
		--go-grpc_out=. --go-grpc_opt=module=github/nallanos/fire2 \
		proto/worker/v1/worker.proto \
		proto/orchestrator/v1/orchestrator.proto

.PHONY: run
run:
	$(GO) run ./cmd/api

.PHONY: run-worker
run-worker:
	$(GO) run ./cmd/worker

ANSIBLE_INVENTORY ?= ansible/inventory/hosts.yml
ANSIBLE_PLAYBOOK  ?= ansible/playbook.yml
ANSIBLE_CMD       ?= .venv/bin/ansible-playbook

.PHONY: ansible-install
ansible-install:
	python3 -m virtualenv .venv
	.venv/bin/pip install -r ansible/requirements.txt

# ansible-build/ansible-deploy target the workers group (ansible/playbook.yml).
# See orchestrator-build/orchestrator-deploy below for the API/orchestrator
# layer — kept as separate targets so deploying one never silently redeploys
# the other.
.PHONY: ansible-build
ansible-build:
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o dist/worker ./cmd/worker
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o dist/fire2-shim ./cmd/fire2-shim
	@echo "built: dist/worker dist/fire2-shim"

.PHONY: ansible-deploy
ansible-deploy:
	$(ANSIBLE_CMD) $(ANSIBLE_PLAYBOOK) -i $(ANSIBLE_INVENTORY)

# Deploys the orchestrator API binary (ansible/roles/fire2_orchestrator) —
# same cross-compile/scp/systemd pattern as the worker role. Needs
# PROD_DATABASE_URL and TAILSCALE_AUTHKEY in .env, and a real
# ansible_ssh_private_key_file for the orchestrator host in
# ansible/inventory/hosts.yml (a placeholder is there until you fill it in).
ORCHESTRATOR_PLAYBOOK ?= ansible/orchestrator-playbook.yml

.PHONY: orchestrator-build
orchestrator-build:
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o dist/api ./cmd/api
	@echo "built: dist/api"

.PHONY: orchestrator-deploy
orchestrator-deploy:
	$(ANSIBLE_CMD) $(ORCHESTRATOR_PLAYBOOK) -i $(ANSIBLE_INVENTORY)

# Applies pending migrations (internal/db/migrations) to the prod database
# by running dbmate on MainDev — the DB only listens there. Same tasks run
# as part of orchestrator-deploy, before the binary is replaced; this target
# runs them alone. Add ANSIBLE_ARGS="--check" to preview.
.PHONY: db-migrate-prod
db-migrate-prod:
	$(ANSIBLE_CMD) $(ORCHESTRATOR_PLAYBOOK) -i $(ANSIBLE_INVENTORY) --tags migrate $(ANSIBLE_ARGS)

# Smoke-test sandbox creation against a live API. API_HOST defaults to prod
# (see make/sandbox.mk) — override on the command line to hit local instead:
#   make ansible-smoke API_HOST=localhost:8081
.PHONY: ansible-smoke
ansible-smoke: sandbox-token
	@echo "hitting API at $(API_HOST)"
	curl -sS -X POST http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","ttl":3600}' | cat
	curl -sS http://$(API_HOST)/api/sandboxes \
		-H "Authorization: Bearer $$(cat $(SANDBOX_TOKEN_FILE))" | cat

# One-command real Firecracker boot test: builds cmd/firecracker-smoke for
# linux/amd64, ships it to FC_HOST, runs it against whatever kernel/rootfs
# live in FC_IMAGE_DIR there, and cleans up after itself either way.
# Override for a host provisioned via the real Ansible path once
# firecracker_kernel_url/firecracker_rootfs_url are set:
#   make firecracker-smoke FC_IMAGE_DIR=/opt/fire2/firecracker/images
FC_HOST      ?= 167.99.38.141
FC_SSH_KEY   ?= ~/.ssh/fire2_worker_deploy
FC_IMAGE_DIR ?= /opt/fc-test

.PHONY: firecracker-smoke
firecracker-smoke:
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o dist/firecracker-smoke ./cmd/firecracker-smoke
	scp -i $(FC_SSH_KEY) -o StrictHostKeyChecking=accept-new dist/firecracker-smoke root@$(FC_HOST):/tmp/firecracker-smoke
	ssh -i $(FC_SSH_KEY) root@$(FC_HOST) '\
		chmod +x /tmp/firecracker-smoke; \
		FIRECRACKER_IMAGE_DIR=$(FC_IMAGE_DIR) /tmp/firecracker-smoke; \
		code=$$?; rm -f /tmp/firecracker-smoke; exit $$code'
