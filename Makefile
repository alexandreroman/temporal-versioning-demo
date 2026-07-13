.DEFAULT_GOAL := help

# Local-dev overlay: loaded only for dev/test targets so cluster targets
# (deploy/teardown) run against the host environment unchanged.
DEV_TARGETS := dev \
               app-up app-v1 app-v2 app-v3 app-down app-logs \
               infra-up infra-down infra-logs \
               test check
GOALS := $(or $(MAKECMDGOALS),$(.DEFAULT_GOAL))
ifneq (,$(filter $(DEV_TARGETS),$(GOALS)))
ifneq (,$(wildcard .env.local))
include .env.local
export
endif
endif

# Per-worktree port remap: when `make worktree-ports` has written a
# compose.override.yaml (Casper/cmux workspaces), it is the source of truth for
# the published host ports. Read them back so the host-side `dev` flow binds and
# connects to the same ports docker publishes — never the fixed defaults.
ifneq (,$(wildcard compose.override.yaml))
DASHBOARD_PORT     := $(shell sed -nE 's/.*"([0-9]+):8080".*/\1/p' compose.override.yaml | head -n1)
TEMPORAL_GRPC_PORT := $(shell sed -nE 's/.*"([0-9]+):7233".*/\1/p' compose.override.yaml | head -n1)
# Air's live-reload proxy takes the dashboard port; the raw backend it forwards
# to gets its own port (dashboard + 3) so parallel worktrees never collide on it.
PROXY_PORT         := $(DASHBOARD_PORT)
BACKEND_PORT       := $(shell expr $(DASHBOARD_PORT) + 3)
TEMPORAL_ADDRESS   ?= localhost:$(TEMPORAL_GRPC_PORT)
else
PROXY_PORT         := 8090
BACKEND_PORT       := 8080
endif

# Connection settings (override via .env.local or the environment).
TEMPORAL_ADDRESS ?= 127.0.0.1:7233
TEMPORAL_NAMESPACE ?= default
DEPLOYMENT_NAME ?= pizza
PIZZA_TASK_QUEUE ?= pizza

WORKER_BIN := ./bin/worker
BACKEND_BIN := ./bin/backend

# Connection env shared by the host workers run from the `dev` target.
WORKER_ENV = TEMPORAL_ADDRESS=$(TEMPORAL_ADDRESS) TEMPORAL_NAMESPACE=$(TEMPORAL_NAMESPACE) \
             TEMPORAL_DEPLOYMENT_NAME=$(DEPLOYMENT_NAME) PIZZA_TASK_QUEUE=$(PIZZA_TASK_QUEUE)

##@ Infra

.PHONY: infra-up
infra-up: ## Start the Temporal dev server (waits until healthy)
	docker compose up -d --wait temporal

.PHONY: infra-down
infra-down: ## Stop the Temporal dev server
	docker compose stop temporal

.PHONY: infra-logs
infra-logs: ## Follow Temporal dev server logs
	docker compose logs -f temporal

##@ App (host, hot reload)

.PHONY: dev
dev: infra-up dev-stop ## Start Temporal + backend + workers v1/v2/v3, all hot-reloading; open http://localhost:8090
	# Runs the backend and all three worker versions under Air, so every component
	# hot-reloads. The three workers run at once so you can drive arbitrary rollouts
	# (ramp/promote any version) from the dashboard. Each worker overrides Air's
	# tmp_dir/cmd/bin to its own tmp/worker-vN so the instances don't clobber each
	# other's binary.
	# dev-stop pre-flight reclaims the dev ports and reaps orphans from a crashed session.
	# Trap reaps the whole process group (kill 0) on exit/signal — now also on HUP (closed terminal).
	@echo "Dashboard with live reload: http://localhost:$(PROXY_PORT)"
	@trap 'kill 0' EXIT INT TERM HUP; \
		( PORT=$(BACKEND_PORT) TEMPORAL_ADDRESS=$(TEMPORAL_ADDRESS) TEMPORAL_NAMESPACE=$(TEMPORAL_NAMESPACE) \
			TEMPORAL_DEPLOYMENT_NAME=$(DEPLOYMENT_NAME) PIZZA_TASK_QUEUE=$(PIZZA_TASK_QUEUE) \
			air -c .air.toml -proxy.proxy_port $(PROXY_PORT) -proxy.app_port $(BACKEND_PORT); kill 0 ) & \
		for v in v1 v2 v3; do \
			( $(WORKER_ENV) PIZZA_VERSION=$$v TEMPORAL_WORKER_BUILD_ID=$$v-local \
				air -c .air.worker.toml -tmp_dir tmp/worker-$$v \
				-build.cmd "go build -o ./tmp/worker-$$v/worker ./cmd/worker" \
				-build.bin "./tmp/worker-$$v/worker"; kill 0 ) & \
		done; \
		wait

.PHONY: dev-stop
dev-stop: ## Kill orphaned host dev processes (Air backend + workers)
	@pkill -f 'air -c .air.toml' || true
	@pkill -f '$(CURDIR)/tmp/backend' || true
	@pkill -f 'air -c .air.worker.toml' || true
	@pkill -f '$(CURDIR)/tmp/worker-v' || true
	@# Force-free the dev ports: a backend mid graceful-shutdown (SSE keeps
	@# connections open) can hold the dev ports past the pkill above and block restart.
	@# Target only the LISTENING socket so client connections (e.g. open
	@# browser tabs) are not killed.
	@for port in $(PROXY_PORT) $(BACKEND_PORT); do \
		lsof -ti tcp:$$port -sTCP:LISTEN 2>/dev/null | xargs kill -9 2>/dev/null || true; \
	done
	@echo "Stopped host dev processes (best effort)."

##@ Stack (Docker)

.PHONY: app-up
app-up: ## Bring up the full stack in Docker (Temporal + backend + worker v1)
	docker compose up -d --build

.PHONY: app-v1
app-v1: ## Start the v1 worker in the Docker stack (demo: ship/restart v1)
	docker compose up -d --build worker

.PHONY: app-v2
app-v2: ## Start the v2 worker in the Docker stack (demo: ship v2)
	docker compose --profile v2 up -d --build

.PHONY: app-v3
app-v3: ## Start the v3 worker in the Docker stack (demo: ship v3)
	docker compose --profile v3 up -d --build

.PHONY: app-down
app-down: ## Tear down the whole Docker stack including the v2/v3 workers
	# Activate the v2/v3 profiles so their workers are removed too: a plain
	# `docker compose down` leaves profiled containers running.
	docker compose --profile v2 --profile v3 down

.PHONY: app-logs
app-logs: ## Follow logs from every stack container
	docker compose logs -f

##@ Quality

.PHONY: test
test: ## Run tests (race + shuffle)
	go test -race -shuffle=on ./...

.PHONY: lint
lint: ## Run golangci-lint (requires golangci-lint v2)
	golangci-lint run

.PHONY: format
format: ## Format code (gofumpt + goimports)
	go run mvdan.cc/gofumpt@latest -l -w .
	go run golang.org/x/tools/cmd/goimports@latest -w .

.PHONY: tidy
tidy: ## Tidy and verify module dependencies
	go mod tidy
	go mod verify

.PHONY: check
check: lint test ## Run all checks (lint + test)

##@ Build

.PHONY: build
build: ## Build the worker and backend binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(WORKER_BIN) ./cmd/worker
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BACKEND_BIN) ./cmd/backend

##@ Deploy

.PHONY: deploy
deploy: ## Deploy the demo (v1) to temporal-k8s (images pinned to digests via kbld)
	kubectl kustomize k8s/base | kbld -f - | kubectl apply -f -

.PHONY: deploy-v1
deploy-v1: ## Ship the v1 worker (re-apply the v1 base)
	@$(MAKE) deploy

.PHONY: deploy-v2
deploy-v2: ## Ship the v2 worker (overlay k8s/v2, digest-pinned via kbld)
	kubectl kustomize k8s/v2 | kbld -f - | kubectl apply -f -

.PHONY: deploy-v3
deploy-v3: ## Ship the v3 worker (overlay k8s/v3, digest-pinned via kbld)
	kubectl kustomize k8s/v3 | kbld -f - | kubectl apply -f -

.PHONY: teardown
teardown: ## Remove the demo from the cluster
	# Deletion is by resource identity, so digests are irrelevant — no kbld pass.
	kubectl delete -k k8s/base

##@ Worktrees

.PHONY: worktree-ports
worktree-ports: ## Remap host ports off CASPER_PORT so parallel Casper worktrees don't collide
	@if [ -z "$$CASPER_PORT" ]; then \
		echo "worktree-ports: CASPER_PORT not set; keeping the default host ports."; \
	else \
		printf '# Generated by `make worktree-ports` — do not edit, do not commit.\nname: casper-%s\n\nservices:\n  backend:\n    ports: !override\n      - "%s:8080" # Pizza Tracker http://localhost:%s\n\n  temporal:\n    ports: !override\n      - "%s:7233" # workers & clients gRPC\n      - "%s:8233" # Web UI http://localhost:%s\n' \
			"$${CASPER_WORKSPACE_ID:-workspace}" \
			"$$CASPER_PORT" "$$CASPER_PORT" \
			$$((CASPER_PORT + 1)) $$((CASPER_PORT + 2)) $$((CASPER_PORT + 2)) \
			> compose.override.yaml; \
		echo "worktree-ports: wrote compose.override.yaml (Pizza Tracker on http://localhost:$$CASPER_PORT)"; \
	fi

##@ Helpers

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(firstword $(MAKEFILE_LIST))
