.PHONY: up down seed seed-full verify smoke topology-test integration-test

up:
	docker compose up -d --build --wait

down:
	docker compose --profile expansion down -v --remove-orphans

seed:
	go run ./cmd/seed --scale=dev

seed-full:
	go run ./cmd/seed --scale=full

verify:
	bash test/verify.sh

topology-test:
	bash test/topology.sh

integration-test:
	go test -tags=integration -p 1 ./test/integration/...

smoke: verify
