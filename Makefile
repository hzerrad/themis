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

# Images

IMAGE_TAG ?= dev

.PHONY: images
images: ## Build the core, executor and intelligence images
	docker build -f build/Dockerfile --build-arg BIN=themis-core -t themis-core:$(IMAGE_TAG) .
	docker build -f build/Dockerfile --build-arg BIN=themis-executor -t themis-executor:$(IMAGE_TAG) .
	docker build -f build/intelligence.Dockerfile -t themis-intelligence:$(IMAGE_TAG) .

.PHONY: kind-load
kind-load: images ## Build the images and load them into the kind cluster
	$(GO_TOOL) kind load docker-image --name $(KIND_CLUSTER) themis-core:$(IMAGE_TAG) themis-executor:$(IMAGE_TAG) themis-intelligence:$(IMAGE_TAG)

# End-to-end test

.PHONY: e2e
e2e: ## Run the bad-rollout scenarios on a fresh kind cluster with a fake model
	test/e2e/run.sh

# Local cluster

.PHONY: kind-up kind-down
kind-up: ## Create the local kind cluster
	$(GO_TOOL) kind create cluster --name $(KIND_CLUSTER) --config hack/kind-config.yaml

kind-down: ## Delete the local kind cluster
	$(GO_TOOL) kind delete cluster --name $(KIND_CLUSTER)

# Themis and monitoring on the kind cluster. Bring your own model:
# make themis-up MODEL_URL=https://... MODEL_NAME=... [MODEL_KEY_SECRET=...]
MODEL_URL        ?=
MODEL_KEY_SECRET ?=
PROMETHEUS_CHART := 29.39.0

.PHONY: themis-up themis-down
themis-up: kind-load ## Install Themis on kind with the executor, using MODEL_URL and MODEL_NAME
	@test -n "$(MODEL_URL)" || { echo "set MODEL_URL (and MODEL_NAME) to an OpenAI-compatible endpoint"; exit 1; }
	helm --kube-context kind-$(KIND_CLUSTER) upgrade --install themis deploy/helm/themis -n themis --create-namespace \
		--set executor.enabled=true --set model.baseURL=$(MODEL_URL) --set model.name=$(MODEL_NAME) \
		$(if $(MODEL_KEY_SECRET),--set model.apiKeySecret=$(MODEL_KEY_SECRET)) --wait --timeout 5m

themis-down: ## Uninstall Themis from kind
	helm --kube-context kind-$(KIND_CLUSTER) uninstall themis -n themis

.PHONY: ui
ui: ## Open the in-cluster Themis UI on http://127.0.0.1:8090 and print its token
	@echo "Admin token: $$($(KUBECTL) -n themis get secret themis-tokens -o jsonpath='{.data.admin}' | base64 -d)"
	@echo "UI: http://127.0.0.1:8090"
	$(KUBECTL) -n themis port-forward svc/themis-core 8090:8080

.PHONY: monitoring-up monitoring-down
monitoring-up: ## Install Prometheus and Alertmanager on kind, alerting the in-cluster Themis
	$(KUBECTL) create namespace monitoring --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) -n themis get secret themis-tokens -o jsonpath='{.data.intake}' | base64 -d | \
		$(KUBECTL) -n monitoring create secret generic themis-intake-token --from-file=token=/dev/stdin \
		--dry-run=client -o yaml | $(KUBECTL) apply -f -
	helm --kube-context kind-$(KIND_CLUSTER) upgrade --install monitoring prometheus-community/prometheus \
		--version $(PROMETHEUS_CHART) -n monitoring -f hack/dev/monitoring/values.yaml --wait --timeout 5m

monitoring-down: ## Uninstall the monitoring stack from kind
	helm --kube-context kind-$(KIND_CLUSTER) uninstall monitoring -n monitoring

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
DEV_EXECUTOR_TOKEN     := bin/dev/executor-token
DEV_DATABASE_URL       := postgres://themis:themis@127.0.0.1:5432/themis?sslmode=disable
MODEL_NAME             ?= qwen3:8b
OLLAMA_URL             ?= http://127.0.0.1:11434/v1

.PHONY: dev-db
dev-db: ## Start a development PostgreSQL on 127.0.0.1:5432
	@docker start themis-postgres >/dev/null 2>&1 || docker run -d --name themis-postgres \
		-e POSTGRES_USER=themis -e POSTGRES_PASSWORD=themis -e POSTGRES_DB=themis \
		-p 127.0.0.1:5432:5432 postgres:17-alpine >/dev/null
	@echo "DATABASE_URL=$(DEV_DATABASE_URL)"

.PHONY: dev-tokens
dev-tokens:
	@mkdir -p $(dir $(DEV_TOKEN))
	@for f in $(DEV_TOKEN) $(DEV_INVESTIGATOR_TOKEN) $(DEV_ADMIN_TOKEN) $(DEV_EXECUTOR_TOKEN); do \
		test -s $$f || { openssl rand -hex 32 > $$f && chmod 600 $$f; }; done

.PHONY: run
run: dev-tokens ## Run themis-core locally against the kind cluster and $DATABASE_URL
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL is not set"; exit 1; }
	go run ./cmd/themis-core --config hack/dev/themis.yaml --kube-context kind-$(KIND_CLUSTER) \
		--http-addr 127.0.0.1:8080 --grpc-addr 127.0.0.1:9090 --probe-addr 127.0.0.1:8081 --metrics-addr 127.0.0.1:8082

.PHONY: run-executor
run-executor: dev-tokens ## Run themis-executor locally against the kind cluster and $DATABASE_URL
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL is not set"; exit 1; }
	go run ./cmd/themis-executor --kube-context kind-$(KIND_CLUSTER) --token-file $(DEV_EXECUTOR_TOKEN) \
		--grpc-addr 127.0.0.1:9091 --probe-addr 127.0.0.1:8083

.PHONY: run-intelligence
run-intelligence: dev-tokens ## Run the investigator against a local Ollama
	THEMIS_CORE_ADDR=127.0.0.1:9090 THEMIS_TOKEN_FILE=$(CURDIR)/$(DEV_INVESTIGATOR_TOKEN) \
		MODEL_BASE_URL=$(OLLAMA_URL) MODEL_NAME=$(MODEL_NAME) MODEL_ENDPOINT=ollama \
		$(UV) run python -m themis_intelligence

.PHONY: demo-alert
demo-alert: ## Post an alert for the crash-looping checkout pod to `make run`
	KIND_CLUSTER=$(KIND_CLUSTER) hack/dev/demo-alert.sh

.PHONY: send-alert
send-alert: ## Post the sample Alertmanager payload to a running `make run`
	curl -sS -X POST http://localhost:8080/v1/intake/alertmanager \
		-H "Authorization: Bearer $$(cat $(DEV_TOKEN))" \
		-H "Content-Type: application/json" \
		--data @test/fixtures/alerts/kube-pod-crashlooping.json
	@echo
