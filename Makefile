.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# --- verification -----------------------------------------------------------

.PHONY: fmt
fmt: ## Format the code
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (if installed)
	golangci-lint run ./...

.PHONY: test
test: ## Run the unit and boundary tests
	go test ./internal/... ./test/boundaries/... -count=1

.PHONY: test-race
test-race: ## Run the unit tests under the race detector
	go test ./internal/... ./test/boundaries/... -race -count=1

.PHONY: test-integration
test-integration: ## Run the integration tests against a real PostgreSQL (Docker required)
	go test ./test/integration/... -count=1 -timeout 10m

.PHONY: test-all
test-all: fmt-check vet test test-integration ## Run every check

.PHONY: cover
cover: ## Report unit test coverage
	go test ./internal/... -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1

# --- build ------------------------------------------------------------------

.PHONY: build
build: ## Build the binary
	go build -o bin/ironledger ./cmd/ironledger

.PHONY: docker-build
docker-build: ## Build the container image
	docker build -t ironledger:dev --build-arg VERSION=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev) .

# --- local development ------------------------------------------------------

.PHONY: up
up: ## Start the local dependencies (postgres, localstack, keycloak)
	docker compose up -d
	@echo "waiting for postgres..."
	@until docker compose exec -T postgres pg_isready -U ironledger -d ironledger >/dev/null 2>&1; do sleep 1; done
	@echo "dependencies ready"

.PHONY: down
down: ## Stop the local dependencies
	docker compose down

.PHONY: reset
reset: ## Stop the dependencies and delete their volumes
	docker compose down -v

.PHONY: run
run: ## Run the API against the local dependencies
	@set -a; [ -f .env ] && . ./.env; set +a; \
	IRONLEDGER_RUN_MIGRATIONS=$${IRONLEDGER_RUN_MIGRATIONS:-true} go run ./cmd/ironledger

.PHONY: token
token: ## Fetch a client-credentials token from the local Keycloak
	@curl -s -X POST \
		"http://localhost:8081/realms/tenant-br/protocol/openid-connect/token" \
		-d grant_type=client_credentials \
		-d client_id=betting-provider \
		-d client_secret=dev-secret | jq -r .access_token
