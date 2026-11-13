SHELL := /usr/bin/env bash

COMPOSE       ?= docker compose
COMPOSE_FILES := -f compose.yaml
COMPOSE_OBS   := -f compose.yaml -f compose.observability.yaml

GATEWAY ?= http://localhost:18000

# Microservice layout: every module directory (lib, core-banking, customer,
# payment, reward, risk, loan, wealth, seed) is its own independent Go project
# (own go.mod); go.work wires them together for root-level commands.
# contracts/ is the generated protobuf module (`bank`) and is exercised through
# the modules that import it; test/integration is built only by
# `make integration-test` (build tag `integration`).
MODULES := lib core-banking customer payment reward risk loan wealth seed

.PHONY: build test cover race up down seed scale config-check topology-test integration-test proto
.PHONY: observability observability-down smoke trace-smoke observability-check

build:
	@for m in $(MODULES); do echo "build $$m"; (cd $$m && go build ./...); done

test:
	@for m in $(MODULES); do echo "test $$m"; (cd $$m && go test ./...); done

# Per-project unit-test coverage: writes coverage.out in each project dir and
# prints the per-project statement total.
cover:
	@set -e; for m in $(MODULES); do \
	  echo "== cover $$m"; \
	  (cd $$m && go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1); \
	done

race:
	cd lib && go test -race ./...

proto:
	docker run --rm -v "$(CURDIR)/contracts:/workspace" -w /workspace bufbuild/buf:1.47.2 generate

up:
	docker compose -f compose.yaml up -d --build --wait
	$(MAKE) seed

down:
	docker compose -f compose.yaml down

seed:
	docker compose -f compose.yaml run --rm seed --scale=$${SCALE:-dev}

scale:
	@test -n "$(SERVICE)" -a -n "$(REPLICAS)"
	docker compose -f compose.yaml up -d --wait --no-deps --scale $(SERVICE)=$(REPLICAS) $(SERVICE)

config-check:
	docker compose -f compose.yaml config --quiet

topology-test:
	bash test/topology.sh

integration-test:
	cd test/integration && go test -tags=integration -p 1 ./...

observability: ## Bring up the otel/prometheus/grafana/jaeger overlay.
	$(COMPOSE) $(COMPOSE_OBS) --profile observability up -d
	@# --wait removed: it races with one-shot migrate containers
	@# (restart: "no") on CI runners.  trace-smoke retries its first
	@# request so services have time to settle.

observability-down: ## Remove the observability overlay containers only.
	$(COMPOSE) $(COMPOSE_OBS) --profile observability down --remove-orphans

trace-smoke: ## Verify a payment request trace reaches Jaeger with REST+gRPC+messaging+workflow spans.
	GATEWAY=$(GATEWAY) bash test/trace-smoke.sh

smoke: ## Run the 10 bank payment saga smoke gates (requires the stack up).
	GATEWAY=$(GATEWAY) bash test/smoke.sh

observability-check: ## Validate dashboard JSON and Prometheus config.
	jq empty deploy/grafana/dashboards/*.json
	docker run --rm --entrypoint /bin/promtool \
	  -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" prom/prometheus:v2.54.1 \
	  check config /etc/prometheus/prometheus.yaml
