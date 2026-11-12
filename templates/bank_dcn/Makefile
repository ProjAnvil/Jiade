# Microservice layout: every module directory (gns, rmb, ..., lib) is its own
# independent Go project (own go.mod); go.work wires them together for
# root-level commands. test/integration is built only by `make integration-test`
# (build tag `integration`).
MODULES := lib gns rmb dcn-app adm batch-scheduler console seed

.PHONY: build test cover up down seed seed-full verify smoke topology-test integration-test

build:
	@for m in $(MODULES); do echo "build $$m"; (cd $$m && go build ./...); done

test:
	@for m in $(MODULES); do echo "test $$m"; (cd $$m && go test ./...); done

# Per-project unit-test coverage: writes coverage.out in each project dir and
# prints the per-project statement total. Env-gated lib tests (MySQL/Redis/
# RabbitMQ) skip cleanly when no broker is up; run with the stack up for full
# coverage (`make up` first).
cover:
	@set -e; for m in $(MODULES); do \
	  echo "== cover $$m"; \
	  (cd $$m && go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1); \
	done

up:
	docker compose up -d --build --wait

down:
	docker compose --profile expansion down -v --remove-orphans

seed:
	go run ./seed/cmd/seed --scale=dev

seed-full:
	go run ./seed/cmd/seed --scale=full

verify:
	bash test/verify.sh

topology-test:
	bash test/topology.sh

integration-test:
	cd test/integration && go test -tags=integration -p 1 ./...

smoke: verify
