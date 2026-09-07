.DEFAULT_GOAL := help
MODULE := $(shell go list -m)
BIN := bin

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the API server and the key CLI into ./bin
	@mkdir -p $(BIN)
	go build -trimpath -ldflags "-s -w" -o $(BIN)/api ./cmd/api
	go build -trimpath -ldflags "-s -w" -o $(BIN)/shortlinkctl ./cmd/shortlinkctl

.PHONY: run
run: ## Run the API server locally (expects Postgres and Redis to be up)
	go run ./cmd/api

.PHONY: test
test: ## Run unit tests (integration tests are skipped without Docker)
	go test -race -short ./...

.PHONY: test-integration
test-integration: ## Run the full suite including testcontainers-backed integration tests
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run unit tests and open an HTML coverage report
	go test -short -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "report: coverage.html"

.PHONY: lint
lint: ## Run golangci-lint (installed on demand)
	@command -v golangci-lint >/dev/null 2>&1 || \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	golangci-lint run ./...

.PHONY: tidy
tidy: ## Sync go.mod/go.sum
	go mod tidy

.PHONY: fmt
fmt: ## Format the code
	go fmt ./...

.PHONY: up
up: ## Start the whole stack (api + postgres + redis) in Docker
	docker compose up --build -d

.PHONY: down
down: ## Stop the stack and drop volumes
	docker compose down -v

.PHONY: logs
logs: ## Tail the API container logs
	docker compose logs -f api

.PHONY: deps-up
deps-up: ## Start only Postgres and Redis (for `make run`)
	docker compose up -d postgres redis

.PHONY: key
key: ## Issue an API key, e.g. make key NAME=local
	go run ./cmd/shortlinkctl issue-key -name "$(or $(NAME),local)"

.PHONY: rename
rename: ## Point the module at your own repo, e.g. make rename MODULE_NEW=github.com/you/shortlink
	@test -n "$(MODULE_NEW)" || (echo "usage: make rename MODULE_NEW=github.com/you/shortlink" && exit 1)
	go mod edit -module $(MODULE_NEW)
	@# .golangci.yml carries the module path too, in the goimports local-prefixes
	@# setting; missing it would silently stop import grouping from working.
	grep -rl '$(MODULE)' --include='*.go' --include='.golangci.yml' . \
		| xargs sed -i.bak 's|$(MODULE)|$(MODULE_NEW)|g'
	find . -name '*.bak' -delete
	go mod tidy
	@echo "module is now $(MODULE_NEW)"

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN) coverage.out coverage.html
