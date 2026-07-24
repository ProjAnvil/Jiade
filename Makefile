.PHONY: generate test bank-test e2e clean

# Pack all built-in templates → templates.tar (required by go:embed)
generate:
	go generate ./internal/template

# jiade itself (without templates/bank - it is an independent module)
test: generate
	go build ./...
	go test ./...

# bank template as standalone module verification (acceptance #2)
bank-test:
	cd templates/bank && go build ./... && go test ./...

# End-to-end smoke (requires docker; acceptance #4/#5)
# Two-stage startup: postgres → seed → then start the service to eliminate startup race conditions
e2e: generate
	rm -rf /tmp/jiade-e2e
	go run ./cmd/jiade init --template bank --dir /tmp/jiade-e2e --force
	cd /tmp/jiade-e2e && docker compose up -d --build postgres
	@until docker compose -f /tmp/jiade-e2e/docker-compose.yaml exec -T postgres pg_isready -U bank >/dev/null 2>&1; do sleep 1; done
	cd /tmp/jiade-e2e && go run ./cmd/seed --scale=dev --reset
	cd /tmp/jiade-e2e && docker compose up -d --build core-banking customer payment
	# Three services healthz (Acceptance #4)
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:18080/healthz
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:18081/healthz
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:18082/healthz
	# core-banking read-only (Spec A)
	curl -sf localhost:18080/api/v1/accounts/D0000000001
	curl -sf "localhost:18080/api/v1/accounts/D0000000001/balance"
	# 2 cross-service HTTP aggregation endpoints (Acceptance #5)
	curl -sf localhost:18081/api/v1/customers/C0000001/accounts
	curl -sf localhost:18082/api/v1/payments/transfers/PT000000000001/parties
	@echo "E2E OK"

clean:
	rm -rf /tmp/jiade-e2e
