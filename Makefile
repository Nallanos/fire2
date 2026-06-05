GO ?= go
PROTOC ?= protoc
COMPOSE ?= docker compose
DBMATE ?= dbmate

include make/sandbox.mk

.PHONY: test

test:
	$(GO) test ./...

.PHONY: proto
proto:
	$(PROTOC) -I proto \
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

.PHONY: ansible-build
ansible-build:
	@mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o dist/worker ./cmd/worker
	@echo "built: dist/worker"

.PHONY: ansible-deploy
ansible-deploy:
	ansible-playbook $(ANSIBLE_PLAYBOOK) -i $(ANSIBLE_INVENTORY)

# Smoke-test sandbox creation against a live API.
# Set API_HOST in .env or override on the command line, e.g.:
#   make ansible-smoke API_HOST=192.0.2.10:8081
.PHONY: ansible-smoke
ansible-smoke:
	curl -sS -X POST http://localhost:$(PORT)/api/sandboxes \
		-H 'Content-Type: application/json' \
		-d '{"runtime":"node","ttl":3600}' | cat
	curl -sS http://localhost:$(PORT)/api/sandboxes | cat
