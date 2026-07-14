.PHONY: up down seed test integration-test

up:
	docker compose up -d --build

down:
	docker compose down

seed:
	go run ./cmd/seed --scale=$${SCALE:-dev} --reset

test:
	go test ./...

integration-test:
	go test -tags=integration ./...
