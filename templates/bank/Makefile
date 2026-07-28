.PHONY: up down seed test integration-test proto

proto:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace bufbuild/buf:1.47.2 generate

up:
	docker compose up -d --build postgres
	$(MAKE) seed
	docker compose up -d --build

down:
	docker compose down

seed:
	go run ./cmd/seed --scale=$${SCALE:-dev} --reset

test:
	go test ./...

integration-test:
	go test -tags=integration -p 1 ./...
