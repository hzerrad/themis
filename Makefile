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
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# Dependencies

.PHONY: deps
deps: ## Download the Go modules of Themis and its tools, retrying when the module proxy fails
	@for i in 1 2 3 4 5; do \
		go mod download && go mod download -modfile=tools/go.mod && exit 0; \
		echo "deps: module download failed (attempt $$i of 5)" >&2; \
		[ $$i -lt 5 ] && sleep $$((i * 10)); \
	done; exit 1

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

.PHONY: eval-bundles
eval-bundles: ## Rebuild every scenario pack case's evaluation bundle
	go test ./eval -run 'TestPacks$$' -count=1 -update

# The model whose replies CI replays.
EVAL_BASELINE := eval/recordings/qwen3-8b
# The model to record or replay, and where to reach it. For a Claude model:
# EVAL_PROVIDER=anthropic EVAL_MODEL=claude-sonnet-5-5 EVAL_KEY_FILE=<file holding the key>.
EVAL_PROVIDER ?= openai
EVAL_BASE_URL ?= http://127.0.0.1:11434/v1
EVAL_MODEL    ?= qwen3:8b
EVAL_KEY_FILE ?=
EVAL_RUNS     ?= 1
EVAL_MAX_USD  ?= 5
# Each model's replies and report have a folder of their own, named after it.
EVAL_SLUG     ?= $(subst :,-,$(subst /,-,$(EVAL_MODEL)))
EVAL_SAVED    := eval/recordings/$(EVAL_SLUG)
EVAL_MODEL_FLAGS = --provider $(EVAL_PROVIDER) --model $(EVAL_MODEL) \
	$(if $(filter openai,$(EVAL_PROVIDER)),--base-url $(EVAL_BASE_URL) --context-tokens 16384)

.PHONY: eval-record eval-replay eval-compare
eval-record: ## Run a model on every case, or one pack's with NAME=..., and save its replies and report; review the report before committing
	$(if $(NAME),,rm -rf $(EVAL_SAVED))
	$(UV) run python -m themis_intelligence.evaluation --runs $(EVAL_RUNS) $(EVAL_MODEL_FLAGS) \
		$(if $(EVAL_KEY_FILE),--api-key-file $(abspath $(EVAL_KEY_FILE))) --max-usd $(EVAL_MAX_USD) \
		--step-deadline 600 --record ../$(EVAL_SAVED) --out ../eval/out/models/$(EVAL_SLUG) $(if $(NAME),--pack $(NAME))

eval-replay: ## Replay a model's saved replies (EVAL_MODEL, the baseline by default) through the real loop and scorer: fails on harm, or if the prompt changed since recording
	$(UV) run python -m themis_intelligence.evaluation --runs $(EVAL_RUNS) $(EVAL_MODEL_FLAGS) \
		--replay ../$(EVAL_SAVED) --out ../eval/out/replay/$(EVAL_SLUG) --gate

eval-compare: ## Compare the models recorded with eval-record, from their reports in eval/out/models
	@$(UV) run python -m themis_intelligence.evaluation.compare $(abspath $(wildcard eval/out/models/*))

# Scenario packs: one folder each under scenarios/; see docs/scenarios.md.

.PHONY: scenario scenario-check
scenario: ## Create a scenario pack to fill in: make scenario NAME=my-scenario
	@test -n "$(NAME)" || { echo "set NAME, for example make scenario NAME=my-scenario"; exit 1; }
	@go run ./scripts/scenario new $(NAME)

scenario-check: ## Check every scenario pack, or one with NAME=..., and replay its cases: fails until it is complete
	go run ./scripts/scenario check $(NAME)
	$(UV) run python -m themis_intelligence.evaluation --runs 1 --replay ../$(EVAL_BASELINE) --out ../eval/out/scenario-check \
		--gate $(if $(NAME),--pack $(NAME))

.PHONY: eval-local
eval-local: ## Evaluate a local model on every scenario; start Ollama with OLLAMA_CONTEXT_LENGTH=16384
	$(UV) run python -m themis_intelligence.evaluation --out ../eval/out/local --context-tokens 16384

.PHONY: test test-go test-go-next test-python
test: test-go test-python ## Run all unit tests

test-go: envtest ## Run Go tests with the race detector
ifneq ($(HAS_GO),)
	KUBEBUILDER_ASSETS="$$($(GO_TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" go test -race ./...
else
	@echo "test-go: no Go packages yet, skipping"
endif

test-go-next: envtest ## Run Go tests against the next, unreleased Kubernetes (Linux only)
	@# Stop if the script fails: tests without KUBEBUILDER_ASSETS fall back to a released version.
	assets="$$(scripts/envtest-next.sh "$$($(GO_TOOL) setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)")" && \
		KUBEBUILDER_ASSETS="$$assets" go test -race ./...

test-python: ## Run Python tests
	$(UV) run pytest || test $$? -eq 5  # exit code 5 means no tests collected

# Security

.PHONY: vuln vuln-go vuln-python vuln-images
vuln: vuln-go vuln-python ## Scan Go and Python dependencies for known vulnerabilities

vuln-go: ## Report known vulnerabilities the Go code can reach (govulncheck)
	$(GO_TOOL) govulncheck ./...

vuln-python: ## Report known vulnerabilities in the intelligence service's runtime dependencies
	@mkdir -p bin
	$(UV) export --quiet --frozen --no-dev --no-emit-project --format requirements-txt -o $(CURDIR)/bin/intelligence-requirements.txt
	$(UV) run pip-audit --strict --disable-pip -r $(CURDIR)/bin/intelligence-requirements.txt

vuln-images: images ## Scan the images for fixable HIGH and CRITICAL vulnerabilities (needs trivy)
	@status=0; for image in themis-core themis-executor themis-intelligence; do \
		trivy image --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 $$image:$(IMAGE_TAG) || status=1; \
	done; exit $$status

# Code generation

.PHONY: gen
gen: ## Generate code from protobuf contracts and the config schema
	$(GO_TOOL) buf generate
	go run ./scripts/gen-config-schema deploy/config/themis.schema.json deploy/helm/themis/crds
	$(UV) sync --quiet --reinstall-package themis-intelligence  # pick up new generated packages

GENERATED := gen intelligence/gen deploy/config/themis.schema.json deploy/helm/themis/crds

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
	go run ./scripts/migrate

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
	$(GO_TOOL) kind create cluster --name $(KIND_CLUSTER) --config scripts/kind-config.yaml

kind-down: ## Delete the local kind cluster
	$(GO_TOOL) kind delete cluster --name $(KIND_CLUSTER)

# Themis and monitoring on the kind cluster. Bring your own model:
# make themis-up MODEL_URL=https://... MODEL_NAME=... [MODEL_KEY_SECRET=...]
# make themis-up MODEL_PROVIDER=anthropic MODEL_NAME=claude-... MODEL_KEY_SECRET=...
MODEL_URL        ?=
MODEL_KEY_SECRET ?=
PROMETHEUS_CHART := 29.39.0
# The Prometheus server monitoring-up installs, as Themis reaches it: Themis
# reads the containers' memory and CPU figures, such as peaks and throttling,
# from it.
PROMETHEUS_URL   := http://monitoring-prometheus-server.monitoring.svc

.PHONY: themis-up themis-down
themis-up: kind-load ## Install Themis on kind with the executor, reading monitoring-up's Prometheus, using MODEL_URL and MODEL_NAME, or MODEL_PROVIDER=anthropic and MODEL_NAME
	@test -n "$(MODEL_URL)" || test "$(MODEL_PROVIDER)" = anthropic || { echo "Set MODEL_URL and MODEL_NAME for an OpenAI-compatible endpoint, or MODEL_PROVIDER=anthropic and MODEL_NAME for Claude."; exit 1; }
	@# Themis gets its access in each enrolled namespace at install, so it must exist.
	$(KUBECTL) create namespace shop --dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null
	helm --kube-context kind-$(KIND_CLUSTER) upgrade --install themis deploy/helm/themis -n themis --create-namespace \
		--set image.repository= --set image.tag=$(IMAGE_TAG) --set image.pullPolicy=IfNotPresent \
		--set executor.enabled=true --set model.provider=$(MODEL_PROVIDER) \
		$(if $(MODEL_URL),--set model.baseURL=$(MODEL_URL)) --set model.name=$(MODEL_NAME) \
		$(if $(MODEL_KEY_SECRET),--set model.apiKeySecret=$(MODEL_KEY_SECRET)) \
		--set metrics.prometheus.address=$(PROMETHEUS_URL) --wait --timeout 5m

themis-down: ## Uninstall Themis from kind
	helm --kube-context kind-$(KIND_CLUSTER) uninstall themis -n themis

.PHONY: ui
ui: ## Open the in-cluster Themis UI on http://localhost:8090, reconnecting whenever the core restarts
	@echo "UI: http://localhost:8090"
	@echo "Until an admin account exists, the initial admin password signs in to create it:"
	@echo "  $(KUBECTL) -n themis get secret themis-initial-admin -o jsonpath='{.data.password}' | base64 -d"
	@echo "Creating the admin account signs you in with it."
	KIND_CLUSTER=$(KIND_CLUSTER) scripts/dev/ui.sh

.PHONY: monitoring-up monitoring-down
monitoring-up: ## Install Prometheus and Alertmanager on kind; Themis installed after finds Alertmanager and reads it
	helm --kube-context kind-$(KIND_CLUSTER) upgrade --install monitoring prometheus-community/prometheus \
		--version $(PROMETHEUS_CHART) -n monitoring --create-namespace -f scripts/dev/monitoring/values.yaml --wait --timeout 5m

monitoring-down: ## Uninstall the monitoring stack from kind
	helm --kube-context kind-$(KIND_CLUSTER) uninstall monitoring -n monitoring

# A sample workload in namespace shop for each scenario pack, on the kind
# cluster. Every command names the kind context, so the current kubectl
# context, which may be a real cluster, is never touched.
KUBECTL := kubectl --context kind-$(KIND_CLUSTER)
DEMO    := scripts/dev/demo

.PHONY: demo-up demo-down
demo-up: ## Deploy the healthy checkout, catalog and search workloads to kind
	$(KUBECTL) apply -f $(DEMO)/checkout.yaml -f $(DEMO)/catalog.yaml -f $(DEMO)/search.yaml
	for d in checkout catalog-feed catalog search-feed search; do \
		$(KUBECTL) -n shop rollout status deployment/$$d --timeout=120s || exit 1; \
	done

demo-down: ## Remove every sample workload, and namespace shop
	$(KUBECTL) delete -f $(DEMO) --ignore-not-found

# Only checkout.yaml holds namespace shop: deleting another file leaves it.
.PHONY: demo-namespace
demo-namespace:
	@$(KUBECTL) create namespace shop --dry-run=client -o yaml | $(KUBECTL) apply -f - >/dev/null

# rollout-regression, and stuck-pods: checkout. demo-fix undoes either break.
.PHONY: demo-break demo-break-stuck demo-fix
demo-break: ## rollout-regression: roll out a revision of checkout that is OOMKilled on every start
	$(KUBECTL) -n shop set env deployment/checkout MODE=leak

demo-break-stuck: ## stuck-pods: roll checkout out to an image tag that doesn't exist
	$(KUBECTL) -n shop set image deployment/checkout app=busybox:1.73

demo-fix: ## Roll checkout back to its previous revision
	$(KUBECTL) -n shop rollout undo deployment/checkout

# oom-limit-too-low: catalog. The pods restart to load the larger catalog;
# deleting them, unlike a rollout restart, leaves the revision as it was.
.PHONY: demo-break-oom demo-fix-oom
demo-break-oom: demo-namespace ## oom-limit-too-low: double catalog's data, past its memory limit, with no rollout
	$(KUBECTL) apply -f $(DEMO)/catalog.yaml
	$(KUBECTL) -n shop set env deployment/catalog-feed SIZE_MI=64
	$(KUBECTL) -n shop rollout status deployment/catalog-feed --timeout=120s
	$(KUBECTL) -n shop delete pods -l app=catalog

demo-fix-oom: ## Shrink catalog's data back under its memory limit
	$(KUBECTL) -n shop set env deployment/catalog-feed SIZE_MI=32
	$(KUBECTL) -n shop rollout status deployment/catalog-feed --timeout=120s
	$(KUBECTL) -n shop delete pods -l app=catalog

# cpu-throttling: search.
.PHONY: demo-break-cpu demo-fix-cpu
demo-break-cpu: demo-namespace ## cpu-throttling: send search more CPU work than its limit allows, with no rollout
	$(KUBECTL) apply -f $(DEMO)/search.yaml
	$(KUBECTL) -n shop set env deployment/search-feed WORK_MILLICORES=250
	$(KUBECTL) -n shop rollout status deployment/search-feed --timeout=120s

demo-fix-cpu: ## Stop search's CPU work
	$(KUBECTL) -n shop set env deployment/search-feed WORK_MILLICORES=0

# probe-restart-storm: inventory.
.PHONY: demo-break-probes demo-fix-probes
demo-break-probes: demo-namespace ## probe-restart-storm: deploy inventory, whose liveness probe gives up before its health check answers
	$(KUBECTL) apply -f $(DEMO)/inventory.yaml

demo-fix-probes: ## Give inventory's liveness probe the 3 seconds its health check needs
	$(KUBECTL) -n shop patch deployment inventory --type=json \
		-p '[{"op": "replace", "path": "/spec/template/spec/containers/0/livenessProbe/timeoutSeconds", "value": 3}]'

# Running locally

DEV_TOKEN              := bin/dev/intake-token
DEV_INVESTIGATOR_TOKEN := bin/dev/investigator-token
DEV_ADMIN_TOKEN        := bin/dev/admin-token
DEV_EXECUTOR_TOKEN     := bin/dev/executor-token
DEV_ADMIN_PASSWORD     := bin/dev/admin-password
DEV_DATABASE_URL       := postgres://themis:themis@127.0.0.1:5432/themis?sslmode=disable
OLLAMA_URL             ?= http://127.0.0.1:11434/v1
# run-intelligence talks to Ollama unless MODEL_PROVIDER or MODEL_BASE_URL say
# otherwise. anthropic with no MODEL_BASE_URL uses Anthropic's own API. Each
# provider has its own default MODEL_NAME.
MODEL_PROVIDER         ?= openai
ifeq ($(MODEL_PROVIDER),anthropic)
MODEL_BASE_URL         ?=
MODEL_NAME             ?= claude-haiku-5-5
else
MODEL_BASE_URL         ?= $(OLLAMA_URL)
MODEL_NAME             ?= qwen3:8b
endif
# `make run` and `make run-executor` use the dev-db database unless DATABASE_URL is set.
DEV_DB_ENV             = DATABASE_URL="$${DATABASE_URL:-$(DEV_DATABASE_URL)}"

.PHONY: dev-db
dev-db: ## Start a development PostgreSQL on 127.0.0.1:5432
	@docker start themis-postgres >/dev/null 2>&1 || docker run -d --name themis-postgres \
		-e POSTGRES_USER=themis -e POSTGRES_PASSWORD=themis -e POSTGRES_DB=themis \
		-p 127.0.0.1:5432:5432 postgres:17-alpine >/dev/null
	@echo "DATABASE_URL=$(DEV_DATABASE_URL)"

.PHONY: dev-tokens
dev-tokens:
	@mkdir -p $(dir $(DEV_TOKEN))
	@for f in $(DEV_TOKEN) $(DEV_INVESTIGATOR_TOKEN) $(DEV_ADMIN_TOKEN) $(DEV_EXECUTOR_TOKEN) $(DEV_ADMIN_PASSWORD); do \
		test -s $$f || { openssl rand -hex 32 > $$f && chmod 600 $$f; }; done

.PHONY: run
run: dev-tokens ## Run themis-core locally against the kind cluster and the dev-db database
	$(DEV_DB_ENV) go run ./cmd/themis-core --config scripts/dev/themis.yaml --kube-context kind-$(KIND_CLUSTER) \
		--http-addr 127.0.0.1:8080 --grpc-addr 127.0.0.1:9090 --probe-addr 127.0.0.1:8081 \
		--metrics-addr 127.0.0.1:8082

.PHONY: run-executor
run-executor: dev-tokens ## Run themis-executor locally against the kind cluster and the dev-db database
	$(DEV_DB_ENV) go run ./cmd/themis-executor --kube-context kind-$(KIND_CLUSTER) --token-file $(DEV_EXECUTOR_TOKEN) \
		--grpc-addr 127.0.0.1:9091 --probe-addr 127.0.0.1:8091 --metrics-addr 127.0.0.1:8092

# The investigator's /metrics would listen on every interface, so it is off here.
.PHONY: run-intelligence
run-intelligence: dev-tokens ## Run the investigator against a local Ollama, or the model MODEL_* names
	THEMIS_CORE_ADDR=127.0.0.1:9090 THEMIS_TOKEN_FILE=$(CURDIR)/$(DEV_INVESTIGATOR_TOKEN) \
		MODEL_PROVIDER=$(MODEL_PROVIDER) MODEL_BASE_URL=$(MODEL_BASE_URL) MODEL_NAME=$(MODEL_NAME) \
		MODEL_ENDPOINT=$(if $(filter $(OLLAMA_URL),$(MODEL_BASE_URL)),ollama,$(MODEL_PROVIDER)) $(if $(MODEL_API_KEY_FILE),MODEL_API_KEY_FILE=$(abspath $(MODEL_API_KEY_FILE))) \
		$(UV) run python -m themis_intelligence --metrics-port 0

.PHONY: demo-alert
demo-alert: ## Post an alert for the crash-looping checkout pod to `make run`
	KIND_CLUSTER=$(KIND_CLUSTER) scripts/dev/demo-alert.sh

.PHONY: send-alert
send-alert: ## Post the sample Alertmanager payload to a running `make run`
	curl -sS -X POST http://localhost:8080/v1/intake/alertmanager \
		-H "Authorization: Bearer $$(cat $(DEV_TOKEN))" \
		-H "Content-Type: application/json" \
		--data @test/fixtures/alerts/kube-pod-crashlooping.json
	@echo
