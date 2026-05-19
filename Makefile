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
