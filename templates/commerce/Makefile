# Commerce template Makefile.
#
# Targets produce the production-shaped compose topology described in
# docs/superpowers/plans/2026-07-24-commerce-production-topology.md (Task 9).
#
#   make up              build + bring up the topology with default replicas,
#                        run migrations, seed dev data, wait for healthy.
#   make scale           re-scale a single SERVICE to REPLICAS instances.
#   make smoke           run test/smoke.sh against the running topology.
#   make observability   bring up the otel/prometheus/grafana overlay.
#   make seed            (re)seed dev data via the seed CLI.
#   make verify-seed     verify the seeded data integrity.
#   make load            bring up the topology at load scale and seed load.
#   make down            stop and remove all containers, networks, volumes.
#
# Override COMPOSE_PROJECT_NAME / INSTANCE_PREFIX via env if running multiple
# instances side-by-side. The gateway stays on host port 18100.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

COMPOSE          ?= docker compose
COMPOSE_FILES    := -f compose.yaml
COMPOSE_OBS_FILE := -f compose.yaml -f compose.observability.yaml
COMPOSE_LOAD     := -f compose.yaml -f compose.load.yaml
COMPOSE_PROJECT  := commerce

# Approved default replica counts (Task 9 spec).
CATALOG_REPLICAS     ?= 2
CUSTOMER_REPLICAS    ?= 1
INVENTORY_REPLICAS   ?= 2
ORDER_REPLICAS       ?= 2
PAYMENT_REPLICAS     ?= 1
FULFILLMENT_REPLICAS ?= 1

DEFAULT_SCALE := \
	--scale catalog=$(CATALOG_REPLICAS) \
	--scale customer=$(CUSTOMER_REPLICAS) \
	--scale inventory=$(INVENTORY_REPLICAS) \
	--scale order=$(ORDER_REPLICAS) \
	--scale payment=$(PAYMENT_REPLICAS) \
	--scale fulfillment=$(FULFILLMENT_REPLICAS)

GATEWAY ?= http://localhost:18100
SEED    ?= 42

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make <target> [VAR=value]\n\nTargets:\n"} \
	  /^[a-zA-Z_-]+:.*?##/ { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build all service images without starting them.
	$(COMPOSE) $(COMPOSE_FILES) build

.PHONY: up
up: ## Bring up the topology with default replicas, migrations, and seed.
	$(COMPOSE) $(COMPOSE_FILES) up -d --wait $(DEFAULT_SCALE)
	$(MAKE) seed

.PHONY: down
down: ## Stop and remove containers, networks, and volumes.
	$(COMPOSE) $(COMPOSE_FILES) down --volumes --remove-orphans

.PHONY: scale
scale: ## Re-scale a single service: make scale SERVICE=order REPLICAS=4.
	@if [ -z "$(SERVICE)" ] || [ -z "$(REPLICAS)" ]; then \
		echo "usage: make scale SERVICE=<svc> REPLICAS=<n>" >&2; exit 2; \
	fi
	$(COMPOSE) $(COMPOSE_FILES) up -d --wait --no-deps --scale $(SERVICE)=$(REPLICAS) $(SERVICE)

.PHONY: smoke
smoke: ## Run the acceptance smoke script against the gateway.
	GATEWAY=$(GATEWAY) SEED=$(SEED) bash test/smoke.sh

.PHONY: observability
observability: ## Bring up the otel/prometheus/grafana overlay.
	$(COMPOSE) $(COMPOSE_OBS_FILE) --profile observability up -d --wait

.PHONY: observability-down
observability-down: ## Remove the observability overlay containers only.
	$(COMPOSE) $(COMPOSE_OBS_FILE) --profile observability down --remove-orphans

.PHONY: seed
seed: ## Seed dev data via the seed CLI.
	$(COMPOSE) $(COMPOSE_FILES) run --rm \
		-e SEED_SCALE=dev \
		-e SEED_VALUE=$(SEED) \
		seed generate --scale dev --seed $(SEED) --reset

.PHONY: verify-seed
verify-seed: ## Verify seeded data integrity.
	$(COMPOSE) $(COMPOSE_FILES) run --rm seed verify --scale dev --seed $(SEED)

.PHONY: load
load: ## Bring up the topology at load scale and seed 1M orders.
	$(COMPOSE) $(COMPOSE_LOAD) up -d --wait \
		--scale order=4 --scale inventory=3
	$(COMPOSE) $(COMPOSE_LOAD) run --rm \
		seed generate --scale load --seed $(SEED) --reset

.PHONY: config-check
config-check: ## Static validation of all compose files.
	$(COMPOSE) $(COMPOSE_FILES) config --quiet
	$(COMPOSE) $(COMPOSE_OBS_FILE) config --quiet
	$(COMPOSE) $(COMPOSE_LOAD) config --quiet
	bash -n test/smoke.sh

.PHONY: logs
logs: ## Tail logs from all services.
	$(COMPOSE) $(COMPOSE_FILES) logs -f --tail=200

.PHONY: ps
ps: ## Show running services.
	$(COMPOSE) $(COMPOSE_FILES) ps
