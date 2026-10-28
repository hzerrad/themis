# Developer tools (buf, golangci-lint, kind) are pinned in tools/go.mod and
# run through `go tool`, so they never leak into the main module's dependencies.
GO_TOOL      := go tool -modfile=tools/go.mod
UV           := uv --directory intelligence
KIND_CLUSTER ?= themis-dev

# Kubernetes version of the API server and etcd that envtest runs in tests.
ENVTEST_K8S_VERSION ?= 1.37.0
ENVTEST_DIR         := $(CURDIR)/bin/envtest

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

test-go: envtest ## Run Go tests with the race detector
ifneq ($(HAS_GO),)
	KUBEBUILDER_ASSETS="$$($(GO_TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" go test -race ./...
else
	@echo "test-go: no Go packages yet, skipping"
endif

test-python: ## Run Python tests
	$(UV) run pytest || test $$? -eq 5  # exit code 5 means no tests collected

# Code generation

.PHONY: gen
gen: ## Generate code from protobuf contracts and the config schema
	$(GO_TOOL) buf generate
	go run ./hack/gen-config-schema deploy/config/themis.schema.json
	$(UV) sync --quiet --reinstall-package themis-intelligence  # pick up new generated packages

GENERATED := gen intelligence/gen deploy/config/themis.schema.json

.PHONY: check-gen
check-gen: gen ## Fail if committed generated code is out of date
	@if [ -n "$$(git status --porcelain -- $(GENERATED))" ]; then \
		echo "Generated code is out of date. Run 'make gen' and commit the result:"; \
		git status --porcelain -- $(GENERATED); \
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

.PHONY: migrate
migrate: ## Apply all migrations (Themis and River) to $DATABASE_URL
	go run ./hack/migrate

# Kubernetes test environment

.PHONY: envtest
envtest: ## Download the API server and etcd binaries used by envtest
	@$(GO_TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) >/dev/null

# Local cluster

.PHONY: kind-up kind-down
kind-up: ## Create the local kind cluster
	$(GO_TOOL) kind create cluster --name $(KIND_CLUSTER) --config hack/kind-config.yaml

kind-down: ## Delete the local kind cluster
	$(GO_TOOL) kind delete cluster --name $(KIND_CLUSTER)

# Scenario 1 on the kind cluster. Every command names the kind context, so the
# current kubectl context, which may be a real cluster, is never touched.
KUBECTL := kubectl --context kind-$(KIND_CLUSTER)

.PHONY: demo-up demo-break demo-fix demo-down
demo-up: ## Deploy the healthy checkout workload (revision 1) to kind
	$(KUBECTL) apply -f hack/dev/demo/checkout.yaml
	$(KUBECTL) -n shop rollout status deployment/checkout --timeout=120s

demo-break: ## Roll out a revision of checkout that is OOMKilled on every start
	$(KUBECTL) -n shop set env deployment/checkout MODE=leak

demo-fix: ## Roll checkout back to its previous revision
	$(KUBECTL) -n shop rollout undo deployment/checkout

demo-down: ## Remove the checkout workload
	$(KUBECTL) delete -f hack/dev/demo/checkout.yaml --ignore-not-found

# Running locally

DEV_TOKEN              := bin/dev/intake-token
DEV_INVESTIGATOR_TOKEN := bin/dev/investigator-token
DEV_ADMIN_TOKEN        := bin/dev/admin-token

.PHONY: run
run: ## Run themis-core locally against the kind cluster and $DATABASE_URL
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL is not set"; exit 1; }
	@mkdir -p $(dir $(DEV_TOKEN))
	@test -s $(DEV_TOKEN) || { openssl rand -hex 32 > $(DEV_TOKEN) && chmod 600 $(DEV_TOKEN); }
	@test -s $(DEV_INVESTIGATOR_TOKEN) || { openssl rand -hex 32 > $(DEV_INVESTIGATOR_TOKEN) && chmod 600 $(DEV_INVESTIGATOR_TOKEN); }
	@test -s $(DEV_ADMIN_TOKEN) || { openssl rand -hex 32 > $(DEV_ADMIN_TOKEN) && chmod 600 $(DEV_ADMIN_TOKEN); }
	go run ./cmd/themis-core --config hack/dev/themis.yaml --kube-context kind-$(KIND_CLUSTER)

.PHONY: send-alert
send-alert: ## Post the sample Alertmanager payload to a running `make run`
	curl -sS -X POST http://localhost:8080/v1/intake/alertmanager \
		-H "Authorization: Bearer $$(cat $(DEV_TOKEN))" \
		-H "Content-Type: application/json" \
		--data @test/fixtures/alerts/kube-pod-crashlooping.json
	@echo
