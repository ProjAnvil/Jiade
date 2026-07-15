.PHONY: generate test bank-test e2e clean

# 打包 templates/bank → templates.tar（go:embed 需要；改模板后重跑）
generate:
	go generate ./internal/template

# jiade 自身（不含 templates/bank——它是独立 module）
test: generate
	go build ./...
	go test ./...

# bank 模板作为独立 module 验证（验收 #2）
bank-test:
	cd templates/bank && go build ./... && go test ./...

# 端到端冒烟（需 docker；验收 #5）
e2e: generate
	rm -rf /tmp/jiade-e2e
	go run ./cmd/jiade init --template bank --dir /tmp/jiade-e2e --force
	cd /tmp/jiade-e2e && docker compose up -d --build
	cd /tmp/jiade-e2e && go run ./cmd/seed --scale=dev --reset
	sleep 5
	curl -sf localhost:8080/healthz
	curl -sf localhost:8080/api/v1/accounts/D0000000001
	curl -sf "localhost:8080/api/v1/accounts/D0000000001/balance"
	@echo "E2E OK"

clean:
	rm -rf /tmp/jiade-e2e
