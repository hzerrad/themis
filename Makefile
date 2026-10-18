# Developer tools (buf, golangci-lint, kind) are pinned in tools/go.mod and
# run through `go tool`, so they never leak into the main module's dependencies.
GO_TOOL      := go tool -modfile=tools/go.mod
UV           := uv --directory intelligence
KIND_CLUSTER ?= themis-dev

# Some checks fail on an empty project; skip them until there is something to check.
HAS_GO    := $(shell find . -name '*.go' -not -path './tools/*' -not -path './gen/*' -print -quit)
HAS_PROTO := $(shell find api/proto -name '*.proto' -print -quit 2>/dev/null)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# Quality

.PHONY: lint lint-go lint-python lint-proto
lint: lint-go lint-python lint-proto ## Run all linters

lint-go: ## Lint Go code
ifneq ($(HAS_GO),)
	$(GO_TOOL) golangci-lint run ./...
else
	@echo "lint-go: no Go packages yet, skipping"
endif

lint-python: ## Lint and type-check Python code
	$(UV) run ruff check
	$(UV) run ruff format --check
	$(UV) run pyright

lint-proto: ## Lint protobuf contracts
ifneq ($(HAS_PROTO),)
	$(GO_TOOL) buf lint
else
	@echo "lint-proto: no .proto files yet, skipping"
endif

.PHONY: fmt
fmt: ## Format Go and Python code
ifneq ($(HAS_GO),)
	$(GO_TOOL) golangci-lint fmt ./...
endif
	$(UV) run ruff format
	$(UV) run ruff check --fix

.PHONY: test test-go test-python
test: test-go test-python ## Run all unit tests

test-go: ## Run Go tests with the race detector
ifneq ($(HAS_GO),)
	go test -race ./...
else
	@echo "test-go: no Go packages yet, skipping"
endif

test-python: ## Run Python tests
	$(UV) run pytest || test $$? -eq 5  # exit code 5 means no tests collected

# Code generation

.PHONY: gen
gen: ## Generate Go and Python code from protobuf contracts
	$(GO_TOOL) buf generate
	$(UV) sync --quiet --reinstall-package themis-intelligence  # pick up new generated packages

.PHONY: check-gen
check-gen: gen ## Fail if committed generated code is out of date
	@if [ -n "$$(git status --porcelain -- gen intelligence/gen)" ]; then \
		echo "Generated code is out of date. Run 'make gen' and commit the result:"; \
		git status --porcelain -- gen intelligence/gen; \
		exit 1; \
	fi

# Database

.PHONY: migration
migration: ## Create a new SQL migration: make migration name=add_leases
ifndef name
	$(error name is required, e.g. make migration name=add_leases)
endif
	@mkdir -p internal/store/migrations
	$(GO_TOOL) goose -dir internal/store/migrations -s create $(name) sql

# Local cluster

.PHONY: kind-up kind-down
kind-up: ## Create the local kind cluster
	$(GO_TOOL) kind create cluster --name $(KIND_CLUSTER) --config hack/kind-config.yaml

kind-down: ## Delete the local kind cluster
	$(GO_TOOL) kind delete cluster --name $(KIND_CLUSTER)
