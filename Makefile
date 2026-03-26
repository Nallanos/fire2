GO ?= go

.PHONY: test

test:
	$(GO) test ./...

.PHONY: run
run:
	$(GO) run ./cmd/api
