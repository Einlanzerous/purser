SHELL := /bin/bash
GO ?= go
BIN := bin/purser
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/Einlanzerous/purser/internal/version.Version=$(VERSION)
IMAGE ?= ghcr.io/einlanzerous/purser

.PHONY: all build run invite test test-db vet fmt tidy lint docker-build clean help

all: build

build: ## Build the static binary into bin/purser
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/purser

run: build ## Run the HTTP server
	$(BIN) serve

invite: build ## Run an invite (pass ARGS="--name … --email … --to …")
	$(BIN) invite $(ARGS)

test: ## Run unit tests (DB-backed tests skip unless PURSER_TEST_DATABASE_URL is set)
	$(GO) test -race ./...

test-db: ## Spin a throwaway Postgres 16 and run the full suite against it
	@docker rm -f purser-test-pg >/dev/null 2>&1 || true
	@docker run -d --name purser-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=purser_test \
		-p 55432:5432 postgres:16-alpine >/dev/null
	@echo "waiting for postgres..." && sleep 3
	@PURSER_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/purser_test?sslmode=disable" \
		$(GO) test -race ./... ; rc=$$? ; docker rm -f purser-test-pg >/dev/null 2>&1 ; exit $$rc

vet: ## go vet
	$(GO) vet ./...

fmt: ## gofmt -w
	gofmt -w .

tidy: ## go mod tidy
	$(GO) mod tidy

lint: ## golangci-lint (falls back to go vet)
	@which golangci-lint >/dev/null 2>&1 && golangci-lint run || $(GO) vet ./...

docker-build: ## Build the production image
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE):latest .

clean: ## Remove build artifacts
	rm -rf bin

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
