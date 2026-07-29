.PHONY: up down seed scale config-check topology-test test integration-test proto

proto:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace bufbuild/buf:1.47.2 generate

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

test:
	go test ./...

integration-test:
	go test -tags=integration -p 1 ./...
