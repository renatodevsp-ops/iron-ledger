# Local development entry points. Every target is a thin wrapper around a plain
# command, so nothing here is required to build or run the project.

SHELL := /bin/bash
COMPOSE := docker compose
BIN := $(CURDIR)/bin
MIGRATE := DATABASE_URL=$${DATABASE_URL:-postgres://iron:iron@localhost:5432/ironledger?sslmode=disable} \
           MIGRATIONS_DIR=migrations go run ./cmd/migrate

.PHONY: help
help: ## Show the available targets.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start the whole platform in Docker.
	$(COMPOSE) up --build -d
	@echo "api      http://localhost:8080"
	@echo "keycloak http://localhost:8081  (admin / admin)"
	@echo "sqs      http://localhost:4566"

.PHONY: down
down: ## Stop the platform and remove its volumes.
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Follow the application logs.
	$(COMPOSE) logs -f api worker

.PHONY: infra
infra: ## Start only the dependencies, for running the binaries on the host.
	$(COMPOSE) up -d postgres localstack keycloak

.PHONY: migrate-up
migrate-up: ## Apply every pending migration.
	$(MIGRATE) up

.PHONY: migrate-down
migrate-down: ## Roll back the last migration.
	$(MIGRATE) down 1

.PHONY: migrate-version
migrate-version: ## Print the applied schema version.
	$(MIGRATE) version

.PHONY: build
build: ## Build the three binaries into ./bin.
	@mkdir -p $(BIN)
	go build -trimpath -o $(BIN)/api ./cmd/api
	go build -trimpath -o $(BIN)/worker ./cmd/worker
	go build -trimpath -o $(BIN)/migrate ./cmd/migrate
	go build -trimpath -o $(BIN)/seed ./cmd/seed

.PHONY: fmt
fmt: ## Format the code.
	gofmt -w cmd internal test

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: ## Run the unit tests.
	go test ./...

.PHONY: test-race
test-race: ## Run the unit tests with the race detector.
	go test -race ./...

.PHONY: integration
integration: ## Run the integration tests against real PostgreSQL, Keycloak and SQS.
	$(COMPOSE) up -d postgres localstack keycloak
	@echo "waiting for the dependencies to answer..."
	@until docker compose exec -T postgres pg_isready -U iron -d ironledger >/dev/null 2>&1; \
		do sleep 1; done
	@until curl -fsS http://localhost:4566/_localstack/health >/dev/null 2>&1; \
		do sleep 1; done
	@until curl -fsS http://localhost:8081/realms/ironledger/.well-known/openid-configuration >/dev/null 2>&1; \
		do sleep 2; done
	@docker compose exec -T postgres psql -U iron -d postgres -c 'CREATE DATABASE ironledger_test OWNER iron' \
		>/dev/null 2>&1 || true
	DATABASE_URL=$${TEST_DATABASE_URL:-postgres://iron:iron@localhost:5432/ironledger_test?sslmode=disable} \
	SQS_ENDPOINT=http://localhost:4566 \
	OIDC_ISSUER_URL=http://localhost:8081/realms/ironledger \
	OIDC_AUDIENCE=iron-ledger-api \
	LOG_LEVEL=error \
	go test -count=1 $(TEST_FLAGS) -tags integration ./test/integration/...

.PHONY: integration-race
integration-race: ## Run the integration tests with the race detector.
	$(MAKE) integration TEST_FLAGS=-race

.PHONY: layers
layers: ## Check that the import graph respects the layering in ARCHITECTURE.md.
	@# -count=1 is not optional. This check shells out to `go list`, which Go's
	@# test cache cannot observe, so a cached pass would survive a change that
	@# broke the layering.
	go test -count=1 ./test/architecture/...

.PHONY: check
check: fmt vet layers test-race ## Everything a change must pass before it is reviewed.

.PHONY: clean
clean: ## Remove the built binaries.
	rm -rf $(BIN)
