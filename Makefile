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

# 端到端冒烟（需 docker；验收 #4/#5）
# 两阶段启动：先 postgres → seed 建 3 库 + setup_fdw → 再 3 服务，消除启动竞态
e2e: generate
	rm -rf /tmp/jiade-e2e
	go run ./cmd/jiade init --template bank --dir /tmp/jiade-e2e --force
	cd /tmp/jiade-e2e && docker compose up -d --build postgres
	@until docker compose -f /tmp/jiade-e2e/docker-compose.yaml exec -T postgres pg_isready -U bank >/dev/null 2>&1; do sleep 1; done
	cd /tmp/jiade-e2e && go run ./cmd/seed --scale=dev --reset
	cd /tmp/jiade-e2e && docker compose up -d --build core-banking customer payment
	# 三服务 healthz（验收 #4）
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:8080/healthz
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:8081/healthz
	curl -sf --retry 10 --retry-connrefused --retry-delay 2 localhost:8082/healthz
	# core-banking 只读（Spec A）
	curl -sf localhost:8080/api/v1/accounts/D0000000001
	curl -sf "localhost:8080/api/v1/accounts/D0000000001/balance"
	# 2 个跨库 FDW JOIN 端点（验收 #5）
	curl -sf localhost:8081/api/v1/customers/C0000001/accounts
	curl -sf localhost:8082/api/v1/payments/transfers/PT000000000001/parties
	@echo "E2E OK"

clean:
	rm -rf /tmp/jiade-e2e
