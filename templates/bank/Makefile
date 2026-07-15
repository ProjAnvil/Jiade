.PHONY: up down seed test integration-test

up:
	docker compose up -d --build postgres
	$(MAKE) seed
	docker compose up -d --build core-banking customer payment

down:
	docker compose down

seed:
	go run ./cmd/seed --scale=$${SCALE:-dev} --reset

test:
	go test ./...

integration-test:
	go test -tags=integration ./...
