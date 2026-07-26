# Encore developer commands.
#
# On Windows without make, every recipe below is a one-line command you can run
# directly; the README lists the PowerShell equivalents.

SHELL := /bin/bash
.DEFAULT_GOAL := help

DB_URL ?= postgres://encore:encore@localhost:5432/encore?sslmode=disable
TEST_DB_URL ?= $(DB_URL)
BENCH_RECORDS ?= 1000000

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- development database ---------------------------------------------------

.PHONY: db-up
db-up: ## Start a local PostgreSQL for development
	docker run -d --name encore-dev-db \
		-e POSTGRES_USER=encore -e POSTGRES_PASSWORD=encore -e POSTGRES_DB=encore -e PGTZ=UTC \
		-p 5432:5432 postgres:17-alpine

.PHONY: db-down
db-down: ## Stop and remove the local development database
	-docker rm -f encore-dev-db

.PHONY: migrate
migrate: ## Apply database migrations
	ENCORE_DATABASE_URL="$(DB_URL)" go run ./cmd/encore-migrate up

.PHONY: migrate-status
migrate-status: ## Show applied and pending migrations
	ENCORE_DATABASE_URL="$(DB_URL)" go run ./cmd/encore-migrate status

.PHONY: migrate-reset
migrate-reset: ## Roll the schema all the way down (destroys data)
	ENCORE_DATABASE_URL="$(DB_URL)" go run ./cmd/encore-migrate reset --yes

# --- running ----------------------------------------------------------------

.PHONY: run-api
run-api: ## Run the API server
	go run ./cmd/encore-api

.PHONY: run-worker
run-worker: ## Run the background worker
	go run ./cmd/encore-worker

.PHONY: run-web
run-web: ## Run the Vite dev server
	cd web && npm run dev

.PHONY: up
up: ## Build and start the whole stack with Docker Compose
	docker compose up -d --build

.PHONY: down
down: ## Stop the Docker Compose stack
	docker compose down

.PHONY: logs
logs: ## Follow Docker Compose logs
	docker compose logs -f

# --- quality ----------------------------------------------------------------

.PHONY: fmt
fmt: ## Format Go and web sources
	gofmt -w $$(git ls-files '*.go')
	cd web && npm run format

.PHONY: lint
lint: ## Run all static analysis
	gofmt -l $$(git ls-files '*.go')
	go vet ./...
	staticcheck ./...
	cd web && npm run lint && npm run typecheck

.PHONY: test
test: ## Run unit tests
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration and import tests against a real database
	ENCORE_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -tags=integration -race -count=1 -timeout=20m ./test/...

.PHONY: test-all
test-all: test test-integration ## Run the full test suite

.PHONY: cover
cover: ## Produce an HTML coverage report
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

# --- benchmark --------------------------------------------------------------

.PHONY: bench
bench: ## Import a large synthetic history and report throughput, memory and row counts
	ENCORE_DATABASE_URL="$(DB_URL)" go run ./cmd/encore-bench run \
		--records $(BENCH_RECORDS) --format extended --report bench.json

.PHONY: bench-generate
bench-generate: ## Only generate a synthetic export file
	go run ./cmd/encore-bench generate --records $(BENCH_RECORDS) --out ./bench-out/history.json

.PHONY: build
build: ## Build all binaries into ./bin
	go build -trimpath -o ./bin/ ./cmd/...
