# DCN 架构模板实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 jiade 新增内置模板 `templates/dcn`，仿真 DCN 单元化架构（GNS 路由 + RMB 可靠消息总线 + 多 DCN 单元 + ADM 全局汇总），并通过 verify 脚本证明爆炸半径、跨 DCN 一致性、幂等与在线扩容。

**Architecture:** 独立 Go 模块（`module dcn`），5 个二进制（gns / rmb-coordinator / dcn-app ×N / adm / seed）共享 `internal/platform` 底盘；docker compose 三个 network（idc1 / idc2 / global-net）仿真两 IDC 主库交叉部署；跨 DCN 事务经 RabbitMQ 由自写协调服务编排（注册→分发→回执→超时补偿→崩溃恢复）。

**Tech Stack:** Go 1.22（标准库 net/http）、MySQL 8、Redis 7、RabbitMQ 3.13（amqp091-go）、shopspring/decimal、docker compose v2、bash + jq（验收脚本）。

**Spec:** `docs/superpowers/specs/2026-08-02-dcn-template-design.md`（接口、DDL、端口、verify 用例的唯一依据）。

## Global Constraints

- 任何文件（代码、注释、文档、commit message）**不得出现特定银行机构名称**；架构统一称「DCN 架构」。
- 模板是独立 Go 模块：`templates/dcn/go.mod` 第一行 `module dcn`，`go 1.22`。
- 依赖仅允许：`github.com/go-sql-driver/mysql`、`github.com/redis/go-redis/v9`、`github.com/rabbitmq/amqp091-go`、`github.com/shopspring/decimal`。
- 所有 HTTP 服务监听 `:8080`（容器内），提供 `GET /healthz` 返回 200。
- 金额一律用 `shopspring/decimal`；JSON 传输用字符串（如 `"100.00"`）。
- 消息持久化（`DeliveryMode: amqp.Persistent`）、手动 ack；所有 exchange/queue 声明幂等（启动时重复声明安全）。
- `jiade seed` 硬编码调用 `go run ./cmd/seed --scale=<dev|full> [--reset]`，seed 必须兼容这两个 flag。
- 每个任务结尾的 commit 步骤由执行者按仓库惯例（Conventional Commits）执行。

## File Structure

新建（模板）：

```
templates/dcn/
├── template.yaml                     # jiade 清单
├── go.mod / go.sum                   # module dcn
├── Dockerfile                        # 多阶段，ARG SERVICE
├── compose.yaml                      # 三网络拓扑 + expansion profile
├── Makefile / .env.example / .dockerignore / .gitignore
├── README.md / README.zh-CN.md / ARCHITECTURE.md
├── cmd/{gns,rmb-coordinator,dcn-app,adm,seed}/main.go
├── internal/contracts/messages.go    # 跨服务消息协议 + 纯函数
├── internal/platform/{runx,httpx,mysqlx,redisx,mq,ratelimit}/
├── internal/gns/{server.go,segment.go,segment_test.go}
├── internal/rmb/coordinator.go
├── internal/dcnapp/server.go
├── internal/adm/server.go
├── db/init/{gns,rmb,dcn,adm}/01_init.sql
└── test/{verify.sh,topology.sh}
```

修改（jiade 接入）：`internal/template/pack.go:49`、根 `Makefile`、`.github/workflows/ci.yml`、根 `README.md`、`README.zh-CN.md`、`internal/template/templates.tar`（重新生成）。

---

### Task 1: 模板骨架与拓扑（compose / DDL / Dockerfile / Makefile / topology.sh）

**Files:**
- Create: `templates/dcn/go.mod`、`templates/dcn/Dockerfile`、`templates/dcn/.dockerignore`、`templates/dcn/.gitignore`、`templates/dcn/.env.example`、`templates/dcn/Makefile`、`templates/dcn/template.yaml`
- Create: `templates/dcn/db/init/gns/01_init.sql`、`templates/dcn/db/init/rmb/01_init.sql`、`templates/dcn/db/init/dcn/01_init.sql`、`templates/dcn/db/init/adm/01_init.sql`
- Create: `templates/dcn/compose.yaml`
- Test: `templates/dcn/test/topology.sh`

**Interfaces:**
- Produces: 网络名 `idc1/idc2/global-net`（compose 内引用名），容器名 `gns/gns-db/gns-redis/dcn-rabbitmq/rmb-coordinator/rmb-db/adm/adm-db/dcn01-app/dcn01-db/dcn02-app/dcn02-db/dcn03-app/dcn03-db/dcn04-app/dcn04-db`；端口映射（18080–18091、13306–13312、15672、16379）；env 变量名 `DB_DSN/REDIS_ADDR/AMQP_URL/GNS_ENDPOINT/RMB_ENDPOINT/DCN_ID/RATE_LIMIT_RPS/TX_TIMEOUT_SECONDS`；AMQP 账号 `dcn/dcn123`；MySQL root 密码默认 `dcn123`（`MYSQL_ROOT_PASSWORD` 可覆盖）。后续所有任务依赖这些名字。

- [ ] **Step 1: 创建 go.mod 与工程卫生文件**

`templates/dcn/go.mod`（go.sum 由 Task 2 的 `go mod tidy` 生成）：

```
module dcn

go 1.22
```

`templates/dcn/.dockerignore`：

```
.git
.env
test
docs
*.md
```

`templates/dcn/.gitignore`：

```
.env
```

`templates/dcn/.env.example`：

```
# MySQL root 密码（compose 与 seed --reset 共用）
MYSQL_ROOT_PASSWORD=dcn123
```

`templates/dcn/Makefile`：

```make
.PHONY: up down seed seed-full verify smoke topology-test

up:
	docker compose up -d --build --wait

down:
	docker compose down -v --remove-orphans

seed:
	go run ./cmd/seed --scale=dev

seed-full:
	go run ./cmd/seed --scale=full

verify:
	bash test/verify.sh

topology-test:
	bash test/topology.sh

smoke: verify
```

`templates/dcn/template.yaml`：

```yaml
name: dcn
description: DCN 单元化架构仿真：GNS 全局路由 + RMB 可靠消息总线 + 多 DCN 自包含单元 + ADM 全局汇总核对
version: 0.1.0
databases:
  - { name: gns-db, migrate: db/init/gns }
  - { name: rmb-db, migrate: db/init/rmb }
  - { name: dcn01-db, migrate: db/init/dcn }
  - { name: dcn02-db, migrate: db/init/dcn }
  - { name: dcn03-db, migrate: db/init/dcn }
  - { name: adm-db, migrate: db/init/adm }
services:
  - { name: gns, port: 18080, db: gns-db }
  - { name: rmb-coordinator, port: 18090, db: rmb-db }
  - { name: dcn01-app, port: 18081, db: dcn01-db }
  - { name: dcn02-app, port: 18082, db: dcn02-db }
  - { name: dcn03-app, port: 18083, db: dcn03-db }
  - { name: adm, port: 18091, db: adm-db }
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

- [ ] **Step 2: 编写四个建库 SQL**

`templates/dcn/db/init/gns/01_init.sql`：

```sql
-- GNS 全局路由库
CREATE TABLE route_segment (
  dcn        VARCHAR(16) PRIMARY KEY,
  seg_start  INT NOT NULL,
  seg_end    INT NOT NULL,
  endpoint   VARCHAR(128) NOT NULL,
  status     VARCHAR(16) NOT NULL
);

CREATE TABLE account_route (
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  request_id VARCHAR(64) UNIQUE,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO route_segment (dcn, seg_start, seg_end, endpoint, status) VALUES
  ('dcn01', 1000, 1999, 'http://dcn01-app:8080', 'ACTIVE'),
  ('dcn02', 2000, 2999, 'http://dcn02-app:8080', 'ACTIVE'),
  ('dcn03', 3000, 3999, 'http://dcn03-app:8080', 'ACTIVE');
```

`templates/dcn/db/init/rmb/01_init.sql`：

```sql
-- RMB 事务协调库
CREATE TABLE tx_log (
  tx_id      VARCHAR(64) PRIMARY KEY,
  type       VARCHAR(32) NOT NULL,
  status     VARCHAR(16) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

CREATE TABLE tx_step_log (
  tx_id   VARCHAR(64) NOT NULL,
  step_no INT NOT NULL,
  dcn     VARCHAR(16) NOT NULL,
  action  VARCHAR(32) NOT NULL,
  status  VARCHAR(16) NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (tx_id, step_no)
);
```

`templates/dcn/db/init/dcn/01_init.sql`（四个 DCN 库共用，dcn04 扩容首次启动时自动执行）：

```sql
-- DCN 单元业务库
CREATE TABLE account (
  account_id INT PRIMARY KEY,
  name       VARCHAR(64),
  balance    DECIMAL(18,2) NOT NULL CHECK (balance >= 0)
);

CREATE TABLE journal (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  direction  VARCHAR(8) NOT NULL,
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_tx_acct (tx_id, account_id, direction)
);
```

`templates/dcn/db/init/adm/01_init.sql`：

```sql
-- ADM 全局汇总库
CREATE TABLE event_log (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  dcn        VARCHAR(16) NOT NULL,
  direction  VARCHAR(8) NOT NULL,
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_event (tx_id, account_id, direction)
);

CREATE TABLE global_balance (
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  balance    DECIMAL(18,2) NOT NULL
);
```

- [ ] **Step 3: 编写 Dockerfile**

`templates/dcn/Dockerfile`：

```dockerfile
FROM golang:1.22-alpine AS builder
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/${SERVICE}

FROM alpine:3.20
COPY --from=builder /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
```

- [ ] **Step 4: 编写 compose.yaml（完整拓扑）**

`templates/dcn/compose.yaml`：

```yaml
name: dcn

x-mysql-common: &mysql-common
  image: mysql:8.0
  restart: unless-stopped
  healthcheck:
    test: ["CMD", "mysqladmin", "ping", "-h", "127.0.0.1"]
    interval: 2s
    timeout: 3s
    retries: 30

x-app-health: &app-health
  restart: unless-stopped
  healthcheck:
    test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]
    interval: 3s
    timeout: 3s
    retries: 40

services:
  # ============ 全局区（global-net） ============
  gns-db:
    <<: *mysql-common
    container_name: gns-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: gns_db
    volumes:
      - ./db/init/gns:/docker-entrypoint-initdb.d:ro
      - gns-db-data:/var/lib/mysql
    ports: ["13309:3306"]
    networks: [global-net]

  gns-redis:
    image: redis:7-alpine
    container_name: gns-redis
    restart: unless-stopped
    ports: ["16379:6379"]
    networks: [global-net]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
      timeout: 3s
      retries: 30

  dcn-rabbitmq:
    image: rabbitmq:3.13-management
    container_name: dcn-rabbitmq
    restart: unless-stopped
    environment:
      RABBITMQ_DEFAULT_USER: dcn
      RABBITMQ_DEFAULT_PASS: dcn123
    volumes:
      - rabbitmq-data:/var/lib/rabbitmq
    ports: ["15672:15672"]
    networks: [global-net]
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "-q", "ping"]
      interval: 3s
      timeout: 5s
      retries: 30

  rmb-db:
    <<: *mysql-common
    container_name: rmb-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: rmb_db
    volumes:
      - ./db/init/rmb:/docker-entrypoint-initdb.d:ro
      - rmb-db-data:/var/lib/mysql
    ports: ["13310:3306"]
    networks: [global-net]

  adm-db:
    <<: *mysql-common
    container_name: adm-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: adm_db
    volumes:
      - ./db/init/adm:/docker-entrypoint-initdb.d:ro
      - adm-db-data:/var/lib/mysql
    ports: ["13311:3306"]
    networks: [global-net]

  gns:
    <<: *app-health
    container_name: gns
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: gns }
    environment:
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(gns-db:3306)/gns_db?parseTime=true
      REDIS_ADDR: gns-redis:6379
    ports: ["18080:8080"]
    networks: [global-net]
    depends_on:
      gns-db:
        condition: service_healthy
      gns-redis:
        condition: service_healthy

  rmb-coordinator:
    <<: *app-health
    container_name: rmb-coordinator
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: rmb-coordinator }
    environment:
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(rmb-db:3306)/rmb_db?parseTime=true
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      TX_TIMEOUT_SECONDS: "5"
    ports: ["18090:8080"]
    networks: [global-net]
    depends_on:
      rmb-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy

  adm:
    <<: *app-health
    container_name: adm
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: adm }
    environment:
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(adm-db:3306)/adm_db?parseTime=true
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      GNS_ENDPOINT: http://gns:8080
    ports: ["18091:8080"]
    networks: [global-net]
    depends_on:
      adm-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy
      gns:
        condition: service_started

  # ============ IDC 1（idc1）：dcn01 + dcn03 主库 ============
  dcn01-db:
    <<: *mysql-common
    container_name: dcn01-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: dcn01_db
    volumes:
      - ./db/init/dcn:/docker-entrypoint-initdb.d:ro
      - dcn01-db-data:/var/lib/mysql
    ports: ["13306:3306"]
    networks: [idc1]

  dcn03-db:
    <<: *mysql-common
    container_name: dcn03-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: dcn03_db
    volumes:
      - ./db/init/dcn:/docker-entrypoint-initdb.d:ro
      - dcn03-db-data:/var/lib/mysql
    ports: ["13308:3306"]
    networks: [idc1]

  dcn01-app:
    <<: *app-health
    container_name: dcn01-app
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: dcn-app }
    environment:
      DCN_ID: dcn01
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(dcn01-db:3306)/dcn01_db?parseTime=true
      GNS_ENDPOINT: http://gns:8080
      RMB_ENDPOINT: http://rmb-coordinator:8080
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      RATE_LIMIT_RPS: "200"
    ports: ["18081:8080"]
    networks: [idc1, global-net]
    depends_on:
      dcn01-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy
      gns:
        condition: service_started
      rmb-coordinator:
        condition: service_started

  dcn03-app:
    <<: *app-health
    container_name: dcn03-app
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: dcn-app }
    environment:
      DCN_ID: dcn03
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(dcn03-db:3306)/dcn03_db?parseTime=true
      GNS_ENDPOINT: http://gns:8080
      RMB_ENDPOINT: http://rmb-coordinator:8080
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      RATE_LIMIT_RPS: "200"
    ports: ["18083:8080"]
    networks: [idc1, global-net]
    depends_on:
      dcn03-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy
      gns:
        condition: service_started
      rmb-coordinator:
        condition: service_started

  # ============ IDC 2（idc2）：dcn02 主库 ============
  dcn02-db:
    <<: *mysql-common
    container_name: dcn02-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: dcn02_db
    volumes:
      - ./db/init/dcn:/docker-entrypoint-initdb.d:ro
      - dcn02-db-data:/var/lib/mysql
    ports: ["13307:3306"]
    networks: [idc2]

  dcn02-app:
    <<: *app-health
    container_name: dcn02-app
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: dcn-app }
    environment:
      DCN_ID: dcn02
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(dcn02-db:3306)/dcn02_db?parseTime=true
      GNS_ENDPOINT: http://gns:8080
      RMB_ENDPOINT: http://rmb-coordinator:8080
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      RATE_LIMIT_RPS: "200"
    ports: ["18082:8080"]
    networks: [idc2, global-net]
    depends_on:
      dcn02-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy
      gns:
        condition: service_started
      rmb-coordinator:
        condition: service_started

  # ============ 扩容单元（profile: expansion，默认不启动） ============
  dcn04-db:
    <<: *mysql-common
    container_name: dcn04-db
    profiles: [expansion]
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: dcn04_db
    volumes:
      - ./db/init/dcn:/docker-entrypoint-initdb.d:ro
      - dcn04-db-data:/var/lib/mysql
    ports: ["13312:3306"]
    networks: [idc2]

  dcn04-app:
    <<: *app-health
    container_name: dcn04-app
    profiles: [expansion]
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: dcn-app }
    environment:
      DCN_ID: dcn04
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(dcn04-db:3306)/dcn04_db?parseTime=true
      GNS_ENDPOINT: http://gns:8080
      RMB_ENDPOINT: http://rmb-coordinator:8080
      AMQP_URL: amqp://dcn:dcn123@dcn-rabbitmq:5672/
      RATE_LIMIT_RPS: "200"
    ports: ["18084:8080"]
    networks: [idc2, global-net]
    depends_on:
      dcn04-db:
        condition: service_healthy
      dcn-rabbitmq:
        condition: service_healthy
      gns:
        condition: service_started
      rmb-coordinator:
        condition: service_started

networks:
  idc1:
    name: dcn-idc1
  idc2:
    name: dcn-idc2
  global-net:
    name: dcn-global

volumes:
  gns-db-data:
  rmb-db-data:
  adm-db-data:
  rabbitmq-data:
  dcn01-db-data:
  dcn02-db-data:
  dcn03-db-data:
  dcn04-db-data:
```

注意：YAML merge key 是浅合并，`environment` 不能靠锚点复用，各服务完整显式列出（上面已如此）。

- [ ] **Step 5: 编写拓扑静态验收脚本**

`templates/dcn/test/topology.sh`（可执行，`chmod +x`）：

```bash
#!/usr/bin/env bash
# 静态拓扑契约：三网络、主库交叉部署、DCN 应用双网卡、DCN 库不进全局区、dcn04 扩容 profile
set -euo pipefail
cd "$(dirname "$0")/.."

cfg=$(docker compose --profile expansion config --format json)

check() { # <描述> <jq 断言>
  if echo "$cfg" | jq -e "$2" >/dev/null; then
    echo "PASS: $1"
  else
    echo "FAIL: $1"
    exit 1
  fi
}

check "三网络 idc1/idc2/global-net 存在" \
  '.networks | keys | contains(["global-net", "idc1", "idc2"])'
check "dcn01-db 仅在 idc1" \
  '.services["dcn01-db"].networks | keys == ["idc1"]'
check "dcn03-db 仅在 idc1" \
  '.services["dcn03-db"].networks | keys == ["idc1"]'
check "dcn02-db 仅在 idc2（主库交叉部署）" \
  '.services["dcn02-db"].networks | keys == ["idc2"]'
check "dcn01-app 双网卡 idc1+global-net" \
  '.services["dcn01-app"].networks | keys | sort == ["global-net", "idc1"]'
check "dcn02-app 双网卡 idc2+global-net" \
  '.services["dcn02-app"].networks | keys | sort == ["global-net", "idc2"]'
check "DCN 数据库均不接入 global-net" \
  '[.services["dcn01-db"], .services["dcn02-db"], .services["dcn03-db"], .services["dcn04-db"]] | all(.networks | has("global-net") | not)'
check "全局区服务不接入任何 IDC 网络" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"]] | all(.networks | keys == ["global-net"])'
check "dcn04 在 expansion profile" \
  '.services["dcn04-app"].profiles == ["expansion"] and .services["dcn04-db"].profiles == ["expansion"]'

echo "topology OK"
```

- [ ] **Step 6: 运行拓扑验收**

Run: `cd templates/dcn && bash test/topology.sh`
Expected: 全部 PASS，末尾 `topology OK`（`docker compose config` 不需要运行中的 docker daemon，但需要 docker CLI）。

- [ ] **Step 7: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add template skeleton with two-IDC compose topology"
```

---

### Task 2: platform 底盘与消息契约

**Files:**
- Create: `templates/dcn/internal/platform/runx/runx.go`
- Create: `templates/dcn/internal/platform/httpx/httpx.go`
- Create: `templates/dcn/internal/platform/mysqlx/mysqlx.go`
- Create: `templates/dcn/internal/platform/redisx/redisx.go`
- Create: `templates/dcn/internal/platform/mq/mq.go`
- Create: `templates/dcn/internal/platform/ratelimit/ratelimit.go`
- Create: `templates/dcn/internal/contracts/messages.go`
- Test: `templates/dcn/internal/platform/ratelimit/ratelimit_test.go`、`templates/dcn/internal/contracts/messages_test.go`

**Interfaces:**
- Produces（后续任务只依赖这些签名）:
  - `runx.Env(key, def string) string`、`runx.MustEnv(key string) string`、`runx.Serve(addr string, h http.Handler)`、`runx.RandHex(n int) string`
  - `httpx.JSON(w, code, v)`、`httpx.Error(w, code, msg)`、`httpx.Decode(r, v) error`
  - `mysqlx.Open(dsn string) *sql.DB`、`mysqlx.IsDuplicate(err error) bool`
  - `redisx.Open(addr string) *redis.Client`
  - `mq.Conn`、`mq.Dial(url string) *Conn`、`(*Conn).DeclareTopicExchange(name)`、`(*Conn).DeclareFanoutExchange(name)`、`(*Conn).DeclareQueue(name)`、`(*Conn).Bind(queue, exchange, key)`、`(*Conn).Publish(exchange, key string, body []byte) error`、`(*Conn).Consume(queue string, handler func([]byte) error)`
  - `ratelimit.New(rps float64) *Limiter`、`(*Limiter).Middleware(next http.Handler) http.Handler`
  - `contracts.StepMessage{TxID, StepNo, Action, AccountID, Amount}`、`contracts.Receipt{TxID, StepNo, DCN, Status, Reason}`、`contracts.BalanceEvent{TxID, AccountID, DCN, Direction, Amount}`、`contracts.StepDirection(action) (suffix, dir string, ok bool)`、`contracts.ReverseAction(action) (string, bool)`

- [ ] **Step 1: 先写失败测试**

`templates/dcn/internal/contracts/messages_test.go`：

```go
package contracts

import "testing"

func TestStepDirection(t *testing.T) {
	cases := []struct {
		action, suffix, dir string
		ok                  bool
	}{
		{"DEBIT", "", "DEBIT", true},
		{"CREDIT", "", "CREDIT", true},
		{"COMPENSATE_DEBIT", ":comp", "CREDIT", true},
		{"COMPENSATE_CREDIT", ":comp", "DEBIT", true},
		{"BOGUS", "", "", false},
	}
	for _, c := range cases {
		suffix, dir, ok := StepDirection(c.action)
		if suffix != c.suffix || dir != c.dir || ok != c.ok {
			t.Errorf("StepDirection(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.action, suffix, dir, ok, c.suffix, c.dir, c.ok)
		}
	}
}

func TestReverseAction(t *testing.T) {
	if got, ok := ReverseAction("DEBIT"); !ok || got != "COMPENSATE_DEBIT" {
		t.Errorf("ReverseAction(DEBIT) = %q,%v", got, ok)
	}
	if got, ok := ReverseAction("CREDIT"); !ok || got != "COMPENSATE_CREDIT" {
		t.Errorf("ReverseAction(CREDIT) = %q,%v", got, ok)
	}
	if _, ok := ReverseAction("COMPENSATE_DEBIT"); ok {
		t.Error("ReverseAction(COMPENSATE_DEBIT) should not be ok")
	}
}
```

`templates/dcn/internal/platform/ratelimit/ratelimit_test.go`：

```go
package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterAllowThenDeny(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(2, func() time.Time { return now })

	if !l.Allow() || !l.Allow() {
		t.Fatal("first two calls should be allowed")
	}
	if l.Allow() {
		t.Fatal("third call within same instant should be denied")
	}
	now = now.Add(600 * time.Millisecond) // 补充 1.2 个令牌
	if !l.Allow() {
		t.Fatal("call after refill should be allowed")
	}
}

func TestLimiterBurstCapped(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(1, func() time.Time { return now })
	now = now.Add(time.Hour) // 长时间空闲后突发容量不应超过桶容量 1
	if !l.Allow() {
		t.Fatal("first call should be allowed")
	}
	if l.Allow() {
		t.Fatal("burst should be capped at bucket size")
	}
}
```

Run: `cd templates/dcn && go test ./...`
Expected: FAIL（`undefined: StepDirection`、`undefined: newForTest` 等编译错误）。

- [ ] **Step 2: 实现 contracts 与 platform**

`templates/dcn/internal/contracts/messages.go`：

```go
// Package contracts 定义经消息总线在 RMB / DCN 应用 / ADM 之间传递的协议消息。
package contracts

// StepMessage 是 RMB 协调服务下发给 DCN 的子事务消息。
type StepMessage struct {
	TxID      string `json:"txId"`
	StepNo    int    `json:"stepNo"`
	Action    string `json:"action"` // DEBIT / CREDIT / COMPENSATE_DEBIT / COMPENSATE_CREDIT
	AccountID int    `json:"accountId"`
	Amount    string `json:"amount"`
}

// Receipt 是 DCN 应用回执给 RMB 协调服务的子事务结果。
type Receipt struct {
	TxID   string `json:"txId"`
	StepNo int    `json:"stepNo"`
	DCN    string `json:"dcn"`
	Status string `json:"status"` // DONE / FAILED
	Reason string `json:"reason,omitempty"`
}

// BalanceEvent 是 DCN 应用上报给 ADM 的余额变更事件。
type BalanceEvent struct {
	TxID      string `json:"txId"`
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Direction string `json:"direction"` // DEBIT / CREDIT
	Amount    string `json:"amount"`
}

// StepDirection 把子事务动作映射为 (journal txId 后缀, 资金方向, 是否合法)。
// 补偿动作用 ":comp" 后缀派生 journal 幂等键，与原始子事务互不冲突。
func StepDirection(action string) (string, string, bool) {
	switch action {
	case "DEBIT":
		return "", "DEBIT", true
	case "CREDIT":
		return "", "CREDIT", true
	case "COMPENSATE_DEBIT":
		return ":comp", "CREDIT", true
	case "COMPENSATE_CREDIT":
		return ":comp", "DEBIT", true
	}
	return "", "", false
}

// ReverseAction 返回动作对应的补偿动作。
func ReverseAction(action string) (string, bool) {
	switch action {
	case "DEBIT":
		return "COMPENSATE_DEBIT", true
	case "CREDIT":
		return "COMPENSATE_CREDIT", true
	}
	return "", false
}
```

`templates/dcn/internal/platform/runx/runx.go`：

```go
// Package runx 提供服务启动的公共小工具。
package runx

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
)

// Env 读环境变量，缺省返回 def。
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// MustEnv 读必需环境变量，缺失即退出。
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

// Serve 启动 HTTP 服务（阻塞，出错即退出）。
func Serve(addr string, h http.Handler) {
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}

// RandHex 返回 2n 位十六进制随机串。
func RandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
```

`templates/dcn/internal/platform/httpx/httpx.go`：

```go
// Package httpx 是 JSON HTTP 处理的小助手集。
package httpx

import (
	"encoding/json"
	"net/http"
)

// JSON 以指定状态码写 JSON 响应。
func JSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Error 写 {"error": msg} 错误响应。
func Error(w http.ResponseWriter, code int, msg string) {
	JSON(w, code, map[string]string{"error": msg})
}

// Decode 解析请求 JSON body。
func Decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
```

`templates/dcn/internal/platform/mysqlx/mysqlx.go`：

```go
// Package mysqlx 封装 MySQL 连接（带容器启动等待重试）。
package mysqlx

import (
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
)

// Open 打开并 ping 通数据库（最多等待 60s，容忍 compose 启动顺序）。
func Open(dsn string) *sql.DB {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("open mysql: %v", err)
	}
	for i := 0; i < 60; i++ {
		if err := db.Ping(); err == nil {
			return db
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("mysql not reachable: %s", dsn)
	return nil
}

// IsDuplicate 判断是否为 MySQL 唯一键冲突（1062）。
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
```

`templates/dcn/internal/platform/redisx/redisx.go`：

```go
// Package redisx 封装 Redis 连接（带容器启动等待重试）。
package redisx

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open 建立并 ping 通 Redis 连接（最多等待 60s）。
func Open(addr string) *redis.Client {
	c := redis.NewClient(&redis.Options{Addr: addr})
	for i := 0; i < 60; i++ {
		if err := c.Ping(context.Background()).Err(); err == nil {
			return c
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("redis not reachable: %s", addr)
	return nil
}
```

`templates/dcn/internal/platform/mq/mq.go`：

```go
// Package mq 封装 RabbitMQ：幂等拓扑声明、持久化发布、手动 ack 消费。
package mq

import (
	"context"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Conn 持有连接与一个带锁的发布通道。
type Conn struct {
	conn *amqp.Connection
	mu   sync.Mutex
	pub  *amqp.Channel
}

// Dial 建立连接（最多等待 60s，容忍 compose 启动顺序）。
func Dial(url string) *Conn {
	var conn *amqp.Connection
	var err error
	for i := 0; i < 60; i++ {
		conn, err = amqp.Dial(url)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("amqp dial: %v", err)
	}
	pub, err := conn.Channel()
	if err != nil {
		log.Fatalf("amqp channel: %v", err)
	}
	return &Conn{conn: conn, pub: pub}
}

// DeclareTopicExchange 幂等声明 durable topic exchange。
func (c *Conn) DeclareTopicExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "topic", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareFanoutExchange 幂等声明 durable fanout exchange。
func (c *Conn) DeclareFanoutExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "fanout", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareQueue 幂等声明 durable 队列。
func (c *Conn) DeclareQueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.pub.QueueDeclare(name, true, false, false, false, nil); err != nil {
		log.Fatalf("declare queue %s: %v", name, err)
	}
}

// Bind 幂等绑定队列到 exchange。
func (c *Conn) Bind(queue, exchange, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.QueueBind(queue, key, exchange, false, nil); err != nil {
		log.Fatalf("bind %s -> %s: %v", queue, exchange, err)
	}
}

// Publish 以持久化模式发布消息。
func (c *Conn) Publish(exchange, key string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pub.PublishWithContext(context.Background(), exchange, key, false, false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})
}

// Consume 消费队列：handler 返回 nil 才 ack；返回 error 则 nack 并延迟重新入队
// （用于数据库暂不可用等可重试基础设施错误）。
func (c *Conn) Consume(queue string, handler func([]byte) error) {
	ch, err := c.conn.Channel()
	if err != nil {
		log.Fatalf("consume channel: %v", err)
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("consume %s: %v", queue, err)
	}
	go func() {
		for d := range msgs {
			if err := handler(d.Body); err != nil {
				log.Printf("handler error on %s, requeue: %v", queue, err)
				_ = d.Nack(false, true)
				time.Sleep(time.Second) // 避免热循环
				continue
			}
			_ = d.Ack(false)
		}
	}()
}
```

`templates/dcn/internal/platform/ratelimit/ratelimit.go`：

```go
// Package ratelimit 实现每实例令牌桶限流中间件（仿真接入层职责）。
package ratelimit

import (
	"net/http"
	"sync"
	"time"
)

// Limiter 是容量等于 rps 的令牌桶。
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// New 创建每秒 rps 个令牌的限流器。
func New(rps float64) *Limiter {
	return newForTest(rps, time.Now)
}

func newForTest(rps float64, now func() time.Time) *Limiter {
	return &Limiter{rate: rps, tokens: rps, last: now(), now: now}
}

// Allow 取一个令牌，成功返回 true。
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.rate {
		l.tokens = l.rate
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// Middleware 超限时返回 429。
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow() {
			http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 3: 拉取依赖并跑测试**

Run: `cd templates/dcn && go mod tidy && go test ./...`
Expected: `ok dcn/internal/contracts`、`ok dcn/internal/platform/ratelimit`，其余包无测试文件。

- [ ] **Step 4: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add platform chassis and messaging contracts"
```

---

### Task 3: GNS 服务（路由定位 / 开户 / 号段管理）

**Files:**
- Create: `templates/dcn/internal/gns/segment.go`
- Create: `templates/dcn/internal/gns/server.go`
- Create: `templates/dcn/cmd/gns/main.go`
- Test: `templates/dcn/internal/gns/segment_test.go`

**Interfaces:**
- Consumes: `httpx`、`mysqlx`、`redisx`、`runx`（Task 2）。
- Produces: `gns.Segment{DCN, SegStart, SegEnd, Endpoint, Status}`、`gns.PickSegment(segs []Segment, counts map[string]int) (Segment, bool)`、`gns.NextAccountID(seg Segment, maxID int, hasMax bool) (int, bool)`、`gns.NewServer(db *sql.DB, cache *redis.Client) *Server`、`(*Server).Handler() http.Handler`。HTTP 契约：`GET /locate?accountId=` → `{accountId,dcn,endpoint}`（404 未知账户）；`POST /accounts` `{name,initBalance,requestId?}` → 201 `{accountId,dcn,endpoint}`；`GET /routes` → `[Segment]`；`POST /routes` `{dcn,segStart,segEnd,endpoint}` → 201 / 200 `{status:"exists"}`。

- [ ] **Step 1: 先写失败测试**

`templates/dcn/internal/gns/segment_test.go`：

```go
package gns

import "testing"

func segs() []Segment {
	return []Segment{
		{DCN: "dcn01", SegStart: 1000, SegEnd: 1999, Status: "ACTIVE"},
		{DCN: "dcn02", SegStart: 2000, SegEnd: 2999, Status: "ACTIVE"},
		{DCN: "dcn03", SegStart: 3000, SegEnd: 3999, Status: "DRAINING"},
	}
}

func TestPickSegmentMinCount(t *testing.T) {
	got, ok := PickSegment(segs(), map[string]int{"dcn01": 5, "dcn02": 2})
	if !ok || got.DCN != "dcn02" {
		t.Fatalf("PickSegment = %v,%v, want dcn02", got.DCN, ok)
	}
}

func TestPickSegmentSkipsDrainingAndTieBreaksBySegStart(t *testing.T) {
	got, ok := PickSegment(segs(), map[string]int{"dcn01": 2, "dcn02": 2, "dcn03": 0})
	if !ok || got.DCN != "dcn01" {
		t.Fatalf("PickSegment = %v,%v, want dcn01 (DRAINING 不参与，并列取号段小者)", got.DCN, ok)
	}
}

func TestNextAccountID(t *testing.T) {
	seg := Segment{DCN: "dcn01", SegStart: 1000, SegEnd: 1999}
	if id, ok := NextAccountID(seg, 0, false); !ok || id != 1000 {
		t.Fatalf("empty segment should start at 1000, got %d,%v", id, ok)
	}
	if id, ok := NextAccountID(seg, 1007, true); !ok || id != 1008 {
		t.Fatalf("next after 1007 should be 1008, got %d,%v", id, ok)
	}
	if _, ok := NextAccountID(seg, 1999, true); ok {
		t.Fatal("segment full should return ok=false")
	}
}
```

Run: `cd templates/dcn && go test ./internal/gns/`
Expected: FAIL（`undefined: Segment` 等编译错误）。

- [ ] **Step 2: 实现号段选择纯函数**

`templates/dcn/internal/gns/segment.go`：

```go
package gns

// Segment 是一个 DCN 号段路由记录。
type Segment struct {
	DCN      string `json:"dcn"`
	SegStart int    `json:"segStart"`
	SegEnd   int    `json:"segEnd"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// PickSegment 在 ACTIVE 号段中选账户数最少者（并列取 segStart 最小者）。
func PickSegment(segs []Segment, counts map[string]int) (Segment, bool) {
	var best Segment
	found := false
	for _, s := range segs {
		if s.Status != "ACTIVE" {
			continue
		}
		if !found ||
			counts[s.DCN] < counts[best.DCN] ||
			(counts[s.DCN] == counts[best.DCN] && s.SegStart < best.SegStart) {
			best, found = s, true
		}
	}
	return best, found
}

// NextAccountID 计算段内下一个账号；号段满返回 ok=false。
func NextAccountID(seg Segment, maxID int, hasMax bool) (int, bool) {
	id := seg.SegStart
	if hasMax {
		id = maxID + 1
	}
	if id > seg.SegEnd {
		return 0, false
	}
	return id, true
}
```

- [ ] **Step 3: 实现 GNS HTTP 服务**

`templates/dcn/internal/gns/server.go`：

```go
// Package gns 实现 GNS 全局路由定位服务：客户 → DCN 映射、开户分配、号段管理。
package gns

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mysqlx"
)

const cacheTTL = time.Hour

// Server 是 GNS 服务。
type Server struct {
	db    *sql.DB
	cache *redis.Client
	hc    *http.Client
}

// NewServer 构造 GNS 服务。
func NewServer(db *sql.DB, cache *redis.Client) *Server {
	return &Server{db: db, cache: cache, hc: &http.Client{Timeout: 5 * time.Second}}
}

// Handler 返回路由表。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /locate", s.handleLocate)
	mux.HandleFunc("POST /accounts", s.handleOpenAccount)
	mux.HandleFunc("GET /routes", s.handleListRoutes)
	mux.HandleFunc("POST /routes", s.handleAddRoute)
	return mux
}

// LocateResult 是 /locate 与 /accounts 的响应体。
type LocateResult struct {
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Endpoint  string `json:"endpoint"`
}

// ErrNotFound 表示账户无路由记录。
var ErrNotFound = errors.New("account not routed")

func cacheKey(id int) string { return fmt.Sprintf("route:%d", id) }

// Locate 先查 Redis 缓存，miss 回源 MySQL 并回填。
func (s *Server) Locate(ctx context.Context, id int) (*LocateResult, error) {
	if v, err := s.cache.Get(ctx, cacheKey(id)).Result(); err == nil {
		if parts := strings.SplitN(v, "|", 2); len(parts) == 2 {
			return &LocateResult{AccountID: id, DCN: parts[0], Endpoint: parts[1]}, nil
		}
	}
	res := &LocateResult{AccountID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT ar.dcn, rs.endpoint FROM account_route ar
		 JOIN route_segment rs ON rs.dcn = ar.dcn WHERE ar.account_id = ?`, id).
		Scan(&res.DCN, &res.Endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.cache.Set(ctx, cacheKey(id), res.DCN+"|"+res.Endpoint, cacheTTL)
	return res, nil
}

func (s *Server) handleLocate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("accountId"))
	if err != nil {
		httpx.Error(w, 400, "accountId must be an integer")
		return
	}
	res, err := s.Locate(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, 404, "account not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, res)
}

type openAccountRequest struct {
	Name        string `json:"name"`
	InitBalance string `json:"initBalance"`
	RequestID   string `json:"requestId,omitempty"`
}

func (s *Server) handleOpenAccount(w http.ResponseWriter, r *http.Request) {
	var req openAccountRequest
	if err := httpx.Decode(r, &req); err != nil || req.Name == "" || req.InitBalance == "" {
		httpx.Error(w, 400, "name and initBalance required")
		return
	}
	// 幂等：requestId 命中直接返回首次开户结果
	if req.RequestID != "" {
		if res, err := s.findByRequestID(r.Context(), req.RequestID); err == nil {
			httpx.JSON(w, 200, res)
			return
		}
	}
	segs, err := s.listSegments(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	counts, err := s.accountCounts(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	seg, ok := PickSegment(segs, counts)
	if !ok {
		httpx.Error(w, 503, "no ACTIVE segment")
		return
	}
	id, err := s.allocate(r.Context(), seg, req.RequestID)
	if err != nil {
		httpx.Error(w, 503, err.Error())
		return
	}
	// 调用目标 DCN 建户；失败回滚路由行，保证路由与实体一致
	if err := s.createInDCN(seg.Endpoint, id, req); err != nil {
		_, _ = s.db.Exec(`DELETE FROM account_route WHERE account_id = ?`, id)
		httpx.Error(w, 502, "dcn create account failed: "+err.Error())
		return
	}
	res := &LocateResult{AccountID: id, DCN: seg.DCN, Endpoint: seg.Endpoint}
	s.cache.Set(r.Context(), cacheKey(id), seg.DCN+"|"+seg.Endpoint, cacheTTL)
	httpx.JSON(w, 201, res)
}

func (s *Server) findByRequestID(ctx context.Context, requestID string) (*LocateResult, error) {
	res := &LocateResult{}
	err := s.db.QueryRowContext(ctx,
		`SELECT ar.account_id, ar.dcn, rs.endpoint FROM account_route ar
		 JOIN route_segment rs ON rs.dcn = ar.dcn WHERE ar.request_id = ?`, requestID).
		Scan(&res.AccountID, &res.DCN, &res.Endpoint)
	return res, err
}

func (s *Server) listSegments(ctx context.Context) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT dcn, seg_start, seg_end, endpoint, status FROM route_segment ORDER BY seg_start`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var seg Segment
		if err := rows.Scan(&seg.DCN, &seg.SegStart, &seg.SegEnd, &seg.Endpoint, &seg.Status); err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

func (s *Server) accountCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dcn, COUNT(*) FROM account_route GROUP BY dcn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var dcn string
		var n int
		if err := rows.Scan(&dcn, &n); err != nil {
			return nil, err
		}
		out[dcn] = n
	}
	return out, rows.Err()
}

// allocate 在号段内分配下一个账号；并发冲突重试 5 次。
func (s *Server) allocate(ctx context.Context, seg Segment, requestID string) (int, error) {
	for i := 0; i < 5; i++ {
		var maxID sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MAX(account_id) FROM account_route WHERE dcn = ?`, seg.DCN).Scan(&maxID); err != nil {
			return 0, err
		}
		id, ok := NextAccountID(seg, int(maxID.Int64), maxID.Valid)
		if !ok {
			return 0, fmt.Errorf("segment %s full", seg.DCN)
		}
		var err error
		if requestID != "" {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO account_route (account_id, dcn, request_id) VALUES (?,?,?)`, id, seg.DCN, requestID)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO account_route (account_id, dcn) VALUES (?,?)`, id, seg.DCN)
		}
		if err == nil {
			return id, nil
		}
		if !mysqlx.IsDuplicate(err) {
			return 0, err
		}
	}
	return 0, errors.New("account allocation failed after retries")
}

func (s *Server) createInDCN(endpoint string, id int, req openAccountRequest) error {
	body, _ := json.Marshal(map[string]any{
		"accountId": id, "name": req.Name, "initBalance": req.InitBalance,
	})
	resp, err := s.hc.Post(endpoint+"/accounts", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("dcn returned %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	segs, err := s.listSegments(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, segs)
}

// handleAddRoute 新增号段（扩容入口）；dcn 主键冲突幂等返回 exists。
// 新号段尚无账户，不存在需要失效的缓存；若未来支持修改号段，必须删除受影响账户的 route:* 键。
func (s *Server) handleAddRoute(w http.ResponseWriter, r *http.Request) {
	var seg Segment
	if err := httpx.Decode(r, &seg); err != nil ||
		seg.DCN == "" || seg.Endpoint == "" || seg.SegStart <= 0 || seg.SegEnd < seg.SegStart {
		httpx.Error(w, 400, "dcn, endpoint and a valid segStart..segEnd range required")
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO route_segment (dcn, seg_start, seg_end, endpoint, status) VALUES (?,?,?,?,'ACTIVE')`,
		seg.DCN, seg.SegStart, seg.SegEnd, seg.Endpoint)
	if mysqlx.IsDuplicate(err) {
		httpx.JSON(w, 200, map[string]string{"status": "exists", "dcn": seg.DCN})
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	seg.Status = "ACTIVE"
	httpx.JSON(w, 201, seg)
}
```

- [ ] **Step 4: 编写入口 main**

`templates/dcn/cmd/gns/main.go`：

```go
package main

import (
	"dcn/internal/gns"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/redisx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	cache := redisx.Open(runx.MustEnv("REDIS_ADDR"))
	srv := gns.NewServer(db, cache)
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
```

- [ ] **Step 5: 跑测试与构建**

Run: `cd templates/dcn && go test ./... && go build ./...`
Expected: 全 PASS，构建成功。

- [ ] **Step 6: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add GNS routing service with redis cache and segment allocation"
```

---

### Task 4: DCN 应用（账户 / 转账 / RMB 子事务执行 / ADM 事件）

**Files:**
- Create: `templates/dcn/internal/dcnapp/server.go`
- Create: `templates/dcn/internal/dcnapp/transfer.go`
- Create: `templates/dcn/internal/dcnapp/steps.go`
- Create: `templates/dcn/cmd/dcn-app/main.go`
- Test: `templates/dcn/internal/dcnapp/steps_test.go`

**Interfaces:**
- Consumes: `contracts`、`httpx`、`mq`、`mysqlx`、`ratelimit`、`runx`（Task 2）；GNS HTTP（Task 3）。
- Produces: `dcnapp.NewServer(dcnID string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64) *Server`、`(*Server).Handler() http.Handler`、`(*Server).DeclareAndConsume()`。HTTP 契约：`POST /accounts` `{accountId,name,initBalance}`（幂等）；`GET /accounts/{id}/balance` → `{accountId,balance}`；`GET /internal/balance-sum` → `{dcn,accounts,balanceSum}`；`POST /transfer` `{txId?,fromId,toId,amount}` → 200 `{status:"ok",txId}`（本地或跨 DCN COMMITTED）/ 422 余额不足 / 502 跨 DCN 未提交。RMB 协调服务（Task 5）依赖其行为：消费 `rmb.steps.<dcn>`、回执发布到默认 exchange 的 `rmb.receipts` 队列、事件发布到 `adm.events`。

- [ ] **Step 1: 先写失败测试（applyStep 结果分类的纯函数部分）**

`templates/dcn/internal/dcnapp/steps_test.go`：

```go
package dcnapp

import (
	"errors"
	"testing"
)

// classifyResult 把 applyMovement 的错误映射为 (回执状态, 原因, 是否基础设施错误)。
func TestClassifyResult(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		status     string
		infraError bool
	}{
		{"success", nil, "DONE", false},
		{"duplicate is idempotent DONE", errDuplicate, "DONE", false},
		{"insufficient is business FAILED", errInsufficient, "FAILED", false},
		{"db error is infra (requeue)", errors.New("connection refused"), "", true},
	}
	for _, c := range cases {
		status, _, infra := classifyResult(c.err)
		if status != c.status || infra != c.infraError {
			t.Errorf("%s: classifyResult = (%q, infra=%v), want (%q, infra=%v)",
				c.name, status, infra, c.status, c.infraError)
		}
	}
}
```

Run: `cd templates/dcn && go test ./internal/dcnapp/`
Expected: FAIL（`undefined: classifyResult` 等编译错误）。

- [ ] **Step 2: 实现账户与转账（server.go + transfer.go）**

`templates/dcn/internal/dcnapp/server.go`：

```go
// Package dcnapp 实现 DCN 单元应用：账户业务、DCN 内本地转账、
// RMB 子事务执行与回执、ADM 变更事件上报。dcn01/02/03/04 同构，靠 env 区分。
package dcnapp

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"dcn/internal/contracts"
	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/ratelimit"
)

// Server 是一个 DCN 单元应用。
type Server struct {
	dcn string
	db  *sql.DB
	gns string
	rmb string
	mqc *mq.Conn
	rps float64
	hc  *http.Client
}

// NewServer 构造 DCN 应用。
func NewServer(dcn string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64) *Server {
	return &Server{dcn: dcn, db: db, gns: gns, rmb: rmb, mqc: mqc, rps: rps, hc: newHTTPClient()}
}

// Handler 返回带限流中间件的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /internal/balance-sum", s.handleBalanceSum)
	mux.HandleFunc("POST /transfer", s.handleTransfer)
	return ratelimit.New(s.rps).Middleware(mux)
}

// DeclareAndConsume 声明本单元的 RMB 队列并启动子事务消费。
func (s *Server) DeclareAndConsume() {
	s.mqc.DeclareTopicExchange("rmb.steps")
	s.mqc.DeclareQueue("rmb.steps." + s.dcn)
	s.mqc.Bind("rmb.steps."+s.dcn, "rmb.steps", "step."+s.dcn)
	s.mqc.DeclareFanoutExchange("adm.events")
	s.mqc.Consume("rmb.steps."+s.dcn, s.handleStep)
}

type createAccountRequest struct {
	AccountID   int    `json:"accountId"`
	Name        string `json:"name"`
	InitBalance string `json:"initBalance"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := httpx.Decode(r, &req); err != nil || req.AccountID <= 0 || req.Name == "" {
		httpx.Error(w, 400, "accountId and name required")
		return
	}
	bal, err := decimal.NewFromString(req.InitBalance)
	if err != nil || bal.IsNegative() {
		httpx.Error(w, 400, "initBalance must be a non-negative decimal")
		return
	}
	_, err = s.db.Exec(`INSERT INTO account (account_id, name, balance) VALUES (?,?,?)`,
		req.AccountID, req.Name, bal.String())
	if mysqlx.IsDuplicate(err) {
		httpx.JSON(w, 200, map[string]any{"accountId": req.AccountID, "status": "exists"})
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	// 初始余额计入 ADM 全局镜像
	if bal.GreaterThan(decimal.Zero) {
		s.publishEvent("init-"+strconv.Itoa(req.AccountID), req.AccountID, "CREDIT", bal)
	}
	httpx.JSON(w, 201, map[string]any{"accountId": req.AccountID, "status": "created"})
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, 400, "invalid account id")
		return
	}
	var bal string
	err = s.db.QueryRow(`SELECT balance FROM account WHERE account_id = ?`, id).Scan(&bal)
	if err == sql.ErrNoRows {
		httpx.Error(w, 404, "account not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{"accountId": id, "balance": bal})
}

func (s *Server) handleBalanceSum(w http.ResponseWriter, r *http.Request) {
	var n int
	var sum sql.NullString
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(balance), 0) FROM account`).Scan(&n, &sum); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{"dcn": s.dcn, "accounts": n, "balanceSum": sum.String})
}

// publishEvent 上报余额变更（at-least-once，ADM 端按 uk_event 去重）。
func (s *Server) publishEvent(txID string, accountID int, dir string, amt decimal.Decimal) {
	evt, _ := json.Marshal(contracts.BalanceEvent{
		TxID: txID, AccountID: accountID, DCN: s.dcn, Direction: dir, Amount: amt.String(),
	})
	if err := s.mqc.Publish("adm.events", "", evt); err != nil {
		log.Printf("adm event publish failed (tolerable): %v", err)
	}
}
```

`templates/dcn/internal/dcnapp/transfer.go`：

```go
package dcnapp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

var (
	errDuplicate    = errors.New("duplicate movement")
	errInsufficient = errors.New("insufficient funds")
)

func newHTTPClient() *http.Client {
	// 必须大于 RMB 协调服务的同步等待窗口（10s）
	return &http.Client{Timeout: 15 * time.Second}
}

type transferRequest struct {
	TxID   string `json:"txId,omitempty"`
	FromID int    `json:"fromId"`
	ToID   int    `json:"toId"`
	Amount string `json:"amount"`
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := httpx.Decode(r, &req); err != nil || req.FromID <= 0 || req.ToID <= 0 {
		httpx.Error(w, 400, "fromId and toId required")
		return
	}
	amt, err := decimal.NewFromString(req.Amount)
	if err != nil || !amt.GreaterThan(decimal.Zero) {
		httpx.Error(w, 400, "amount must be a positive decimal")
		return
	}
	from, err := s.locate(req.FromID)
	if err != nil {
		httpx.Error(w, 404, "unknown from account")
		return
	}
	to, err := s.locate(req.ToID)
	if err != nil {
		httpx.Error(w, 404, "unknown to account")
		return
	}
	switch {
	case from.DCN == s.dcn && to.DCN == s.dcn:
		txID, err := s.localTransfer(req.FromID, req.ToID, amt)
		if errors.Is(err, errInsufficient) {
			httpx.Error(w, 422, "insufficient funds")
			return
		}
		if err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		httpx.JSON(w, 200, map[string]string{"status": "ok", "txId": txID})
	case from.DCN == s.dcn:
		s.submitRMB(w, req, to.DCN)
	default:
		// 接入层路由：源账户不在本单元，透明转发到其所属单元（非跨 DCN 业务通信）
		s.forward(w, req, from.Endpoint)
	}
}

type locateResult struct {
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Endpoint  string `json:"endpoint"`
}

func (s *Server) locate(id int) (*locateResult, error) {
	resp, err := s.hc.Get(s.gns + "/locate?accountId=" + strconv.Itoa(id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("not routed")
	}
	var res locateResult
	return &res, json.NewDecoder(resp.Body).Decode(&res)
}

// localTransfer 单库本地事务：条件更新防透支 + 双方 journal（uk_tx_acct 兜底）。
func (s *Server) localTransfer(fromID, toID int, amt decimal.Decimal) (string, error) {
	txID := "local-" + runx.RandHex(8)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := applyMovement(tx, txID, fromID, "DEBIT", amt); err != nil {
		return "", err
	}
	if err := applyMovement(tx, txID, toID, "CREDIT", amt); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.publishEvent(txID, fromID, "DEBIT", amt)
	s.publishEvent(txID, toID, "CREDIT", amt)
	return txID, nil
}

// applyMovement 在一个本地事务内记 journal（幂等键）并变动余额。
func applyMovement(tx *sql.Tx, txID string, accountID int, dir string, amt decimal.Decimal) error {
	if _, err := tx.Exec(
		`INSERT INTO journal (tx_id, account_id, direction, amount) VALUES (?,?,?,?)`,
		txID, accountID, dir, amt.String()); err != nil {
		if mysqlx.IsDuplicate(err) {
			return errDuplicate
		}
		return err
	}
	switch dir {
	case "DEBIT":
		res, err := tx.Exec(
			`UPDATE account SET balance = balance - ? WHERE account_id = ? AND balance >= ?`,
			amt.String(), accountID, amt.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errInsufficient
		}
	case "CREDIT":
		res, err := tx.Exec(
			`UPDATE account SET balance = balance + ? WHERE account_id = ?`, amt.String(), accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errors.New("account not found")
		}
	}
	return nil
}

// submitRMB 注册跨 DCN 总事务并同步等待协调结果。
func (s *Server) submitRMB(w http.ResponseWriter, req transferRequest, toDCN string) {
	payload := map[string]any{
		"type": "TRANSFER",
		"steps": []map[string]any{
			{"dcn": s.dcn, "action": "DEBIT", "accountId": req.FromID, "amount": req.Amount},
			{"dcn": toDCN, "action": "CREDIT", "accountId": req.ToID, "amount": req.Amount},
		},
	}
	if req.TxID != "" {
		payload["txId"] = req.TxID
	}
	body, _ := json.Marshal(payload)
	resp, err := s.hc.Post(s.rmb+"/transactions", "application/json", bytes.NewReader(body))
	if err != nil {
		httpx.Error(w, 502, "rmb unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var result struct {
		TxID   string `json:"txId"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		httpx.Error(w, 502, "invalid rmb response")
		return
	}
	if result.Status == "COMMITTED" {
		httpx.JSON(w, 200, map[string]string{"status": "ok", "txId": result.TxID})
		return
	}
	httpx.JSON(w, 502, map[string]string{
		"error": "transfer not committed", "txId": result.TxID, "status": result.Status,
	})
}

// forward 把请求原样转发到目标单元（接入层职责）。
func (s *Server) forward(w http.ResponseWriter, req transferRequest, endpoint string) {
	body, _ := json.Marshal(req)
	resp, err := s.hc.Post(endpoint+"/transfer", "application/json", bytes.NewReader(body))
	if err != nil {
		httpx.Error(w, 502, "forward failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
```

- [ ] **Step 3: 实现 RMB 子事务执行（steps.go）**

`templates/dcn/internal/dcnapp/steps.go`：

```go
package dcnapp

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/shopspring/decimal"

	"dcn/internal/contracts"
)

// classifyResult 把 applyMovement 的结果映射为回执语义：
// 幂等重复按 DONE 处理；余额不足是业务 FAILED；其余视为基础设施错误（requeue 重试）。
func classifyResult(err error) (status, reason string, infraError bool) {
	switch {
	case err == nil:
		return "DONE", "", false
	case errors.Is(err, errDuplicate):
		return "DONE", "duplicate ignored", false
	case errors.Is(err, errInsufficient):
		return "FAILED", "insufficient funds", false
	default:
		return "", "", true
	}
}

// handleStep 消费一条 RMB 子事务消息。返回非 nil 表示基础设施错误（nack 重投）。
func (s *Server) handleStep(body []byte) error {
	var msg contracts.StepMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("bad step message, drop: %v", err)
		return nil // 毒消息：ack 丢弃
	}
	suffix, dir, ok := contracts.StepDirection(msg.Action)
	if !ok {
		log.Printf("unknown action %q, drop", msg.Action)
		return nil
	}
	amt, err := decimal.NewFromString(msg.Amount)
	if err != nil {
		log.Printf("bad amount %q, drop", msg.Amount)
		return nil
	}
	status, reason, infraErr := s.applyStep(msg.TxID+suffix, msg.AccountID, dir, amt)
	if infraErr != nil {
		return infraErr // nack + requeue
	}
	receipt, _ := json.Marshal(contracts.Receipt{
		TxID: msg.TxID, StepNo: msg.StepNo, DCN: s.dcn, Status: status, Reason: reason,
	})
	if err := s.mqc.Publish("", "rmb.receipts", receipt); err != nil {
		return err // 回执失败也重投（applyStep 幂等，重投安全）
	}
	return nil
}

// applyStep 在本地事务内执行一次资金变动。
func (s *Server) applyStep(journalTxID string, accountID int, dir string, amt decimal.Decimal) (string, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	moveErr := applyMovement(tx, journalTxID, accountID, dir, amt)
	status, reason, isInfra := classifyResult(moveErr)
	if isInfra {
		return "", "", moveErr
	}
	if status == "DONE" && reason == "" {
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		s.publishEvent(journalTxID, accountID, dir, amt)
	}
	return status, reason, nil
}
```

- [ ] **Step 4: 编写入口 main**

`templates/dcn/cmd/dcn-app/main.go`：

```go
package main

import (
	"strconv"

	"dcn/internal/dcnapp"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	dcnID := runx.MustEnv("DCN_ID")
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	rps, err := strconv.ParseFloat(runx.Env("RATE_LIMIT_RPS", "200"), 64)
	if err != nil {
		rps = 200
	}
	srv := dcnapp.NewServer(dcnID, db,
		runx.MustEnv("GNS_ENDPOINT"), runx.MustEnv("RMB_ENDPOINT"), mqc, rps)
	srv.DeclareAndConsume()
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
```

- [ ] **Step 5: 跑测试与构建**

Run: `cd templates/dcn && go test ./... && go build ./...`
Expected: 全 PASS（含 `ok dcn/internal/dcnapp`），构建成功。

- [ ] **Step 6: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add DCN unit app with local transfer, RMB step execution and ADM events"
```

---

### Task 5: RMB 事务协调服务（注册 / 分发 / 回执 / 超时补偿 / 崩溃恢复）

**Files:**
- Create: `templates/dcn/internal/rmb/coordinator.go`
- Create: `templates/dcn/cmd/rmb-coordinator/main.go`

**Interfaces:**
- Consumes: `contracts`、`httpx`、`mq`、`mysqlx`、`runx`（Task 2）。
- Produces: `rmb.NewCoordinator(db *sql.DB, mqc *mq.Conn, timeout time.Duration) *Coordinator`、`(*Coordinator).Handler() http.Handler`、`(*Coordinator).Run()`。HTTP 契约：`POST /transactions` `{type,steps:[{dcn,action,accountId,amount}],txId?}` → `{txId,status}`（同步等待终态，最长 10s）；`GET /transactions/{txId}` → `{txId,type,status,steps:[{stepNo,dcn,action,status}]}`。verify.sh（Task 8）依赖这两个接口与状态机语义：PROCESSING / COMMITTED / COMPENSATED / FAILED。

- [ ] **Step 1: 实现协调服务**

`templates/dcn/internal/rmb/coordinator.go`：

```go
// Package rmb 实现 RMB 可靠消息总线的事务协调服务：
// 跨 DCN 总事务的注册、子事务分发、回执收集、超时补偿与崩溃恢复。
package rmb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dcn/internal/contracts"
	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/runx"
)

const httpWaitWindow = 10 * time.Second

// Coordinator 是 RMB 事务协调服务。
type Coordinator struct {
	db      *sql.DB
	mqc     *mq.Conn
	timeout time.Duration
	attempts sync.Map // 补偿步骤重试计数："txID:stepNo" -> int
}

// NewCoordinator 构造协调服务；timeout 为子事务超时（超时即补偿）。
func NewCoordinator(db *sql.DB, mqc *mq.Conn, timeout time.Duration) *Coordinator {
	return &Coordinator{db: db, mqc: mqc, timeout: timeout}
}

// Handler 返回 HTTP 路由。
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /transactions", c.handleCreate)
	mux.HandleFunc("GET /transactions/{txId}", c.handleGet)
	return mux
}

// Run 声明拓扑、崩溃恢复、启动回执消费与超时器。在 HTTP 服务之前调用。
func (c *Coordinator) Run() {
	c.mqc.DeclareTopicExchange("rmb.steps")
	c.mqc.DeclareQueue("rmb.receipts")
	c.recover()
	c.mqc.Consume("rmb.receipts", c.handleReceipt)
	go c.timeoutLoop()
}

// ---------- 注册与查询 ----------

type txRequest struct {
	TxID  string             `json:"txId,omitempty"`
	Type  string             `json:"type"`
	Steps []txRequestStep    `json:"steps"`
}

type txRequestStep struct {
	DCN       string `json:"dcn"`
	Action    string `json:"action"`
	AccountID int    `json:"accountId"`
	Amount    string `json:"amount"`
}

func (c *Coordinator) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req txRequest
	if err := httpx.Decode(r, &req); err != nil || req.Type == "" || len(req.Steps) == 0 {
		httpx.Error(w, 400, "type and non-empty steps required")
		return
	}
	txID, status, err := c.register(req)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	if status == "PROCESSING" {
		status = c.waitFinal(txID, httpWaitWindow)
	}
	httpx.JSON(w, 200, map[string]string{"txId": txID, "status": status})
}

func (c *Coordinator) handleGet(w http.ResponseWriter, r *http.Request) {
	txID := r.PathValue("txId")
	var typ, status string
	err := c.db.QueryRow(`SELECT type, status FROM tx_log WHERE tx_id = ?`, txID).Scan(&typ, &status)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, 404, "transaction not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := c.db.Query(
		`SELECT step_no, dcn, action, status FROM tx_step_log WHERE tx_id = ? ORDER BY step_no`, txID)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type stepView struct {
		StepNo int    `json:"stepNo"`
		DCN    string `json:"dcn"`
		Action string `json:"action"`
		Status string `json:"status"`
	}
	steps := []stepView{}
	for rows.Next() {
		var sv stepView
		if err := rows.Scan(&sv.StepNo, &sv.DCN, &sv.Action, &sv.Status); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		steps = append(steps, sv)
	}
	httpx.JSON(w, 200, map[string]any{
		"txId": txID, "type": typ, "status": status, "steps": steps,
	})
}

// register 落库总事务与步骤后分发；txId 已存在时幂等返回当前状态。
func (c *Coordinator) register(req txRequest) (string, string, error) {
	txID := req.TxID
	if txID == "" {
		txID = "tx-" + runx.RandHex(8)
	}
	var status string
	err := c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, txID).Scan(&status)
	if err == nil {
		return txID, status, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tx_log (tx_id, type, status) VALUES (?,?,'PROCESSING')`,
		txID, req.Type); err != nil {
		return "", "", err
	}
	for i, st := range req.Steps {
		payload, _ := json.Marshal(contracts.StepMessage{
			TxID: txID, StepNo: i + 1, Action: st.Action, AccountID: st.AccountID, Amount: st.Amount,
		})
		if _, err := tx.Exec(
			`INSERT INTO tx_step_log (tx_id, step_no, dcn, action, status, payload) VALUES (?,?,?,?,'PENDING',?)`,
			txID, i+1, st.DCN, st.Action, payload); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	log.Printf("tx %s: -> PROCESSING (%d steps)", txID, len(req.Steps))
	c.publishPending(txID)
	return txID, "PROCESSING", nil
}

// publishPending 把 PENDING 步骤按 payload 原样投递到各自 DCN 队列。
// DCN 端以 journal 唯一键幂等，重复投递安全。
func (c *Coordinator) publishPending(txID string) {
	rows, err := c.db.Query(
		`SELECT step_no, dcn, payload FROM tx_step_log WHERE tx_id = ? AND status = 'PENDING'`, txID)
	if err != nil {
		log.Printf("tx %s: load pending steps: %v", txID, err)
		return
	}
	type pending struct {
		stepNo  int
		dcn     string
		payload string
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.stepNo, &p.dcn, &p.payload); err == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		if err := c.mqc.Publish("rmb.steps", "step."+p.dcn, []byte(p.payload)); err != nil {
			log.Printf("tx %s step %d: publish failed: %v", txID, p.stepNo, err)
		}
	}
}

// waitFinal 轮询等待事务离开 PROCESSING（最长 d）。
func (c *Coordinator) waitFinal(txID string, d time.Duration) string {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var s string
		if err := c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, txID).Scan(&s); err == nil &&
			s != "PROCESSING" {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "PROCESSING"
}

// ---------- 回执与状态机 ----------

type stepRow struct {
	stepNo  int
	dcn     string
	action  string
	status  string
	payload string
}

func (c *Coordinator) handleReceipt(body []byte) error {
	var rc contracts.Receipt
	if err := json.Unmarshal(body, &rc); err != nil {
		return nil
	}
	var stepStatus, txStatus string
	err := c.db.QueryRow(
		`SELECT status FROM tx_step_log WHERE tx_id = ? AND step_no = ?`, rc.TxID, rc.StepNo).
		Scan(&stepStatus)
	if err != nil {
		return nil // 未知步骤（可能重复回执），丢弃
	}
	_ = c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, rc.TxID).Scan(&txStatus)

	if rc.Status == "DONE" && stepStatus == "FAILED" && txStatus == "COMPENSATED" {
		// 迟到回执再补偿：步骤被判 FAILED（如超时）且事务已 COMPENSATED，
		// 但下游恢复后补执行成功——必须再补一轮反向补偿，保证余额合计不变。
		log.Printf("tx %s: late DONE receipt for FAILED step %d, re-compensating", rc.TxID, rc.StepNo)
		return c.reopenCompensation(rc.TxID, rc.StepNo)
	}

	if rc.Status == "DONE" {
		_, err = c.db.Exec(
			`UPDATE tx_step_log SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'PENDING'`,
			rc.TxID, rc.StepNo)
	} else {
		_, err = c.db.Exec(
			`UPDATE tx_step_log SET status = 'FAILED' WHERE tx_id = ? AND step_no = ? AND status = 'PENDING'`,
			rc.TxID, rc.StepNo)
		log.Printf("tx %s step %d: FAILED (%s)", rc.TxID, rc.StepNo, rc.Reason)
	}
	if err != nil {
		return err
	}
	return c.advance(rc.TxID)
}

// advance 推动事务状态机：全部 DONE → COMMITTED；出现 FAILED → 逆序补偿；补偿齐 → COMPENSATED。
func (c *Coordinator) advance(txID string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ? FOR UPDATE`, txID).
		Scan(&status); err != nil {
		return err
	}
	if status != "PROCESSING" {
		return nil
	}
	steps, err := loadSteps(tx, txID)
	if err != nil {
		return err
	}

	var compSteps []stepRow
	for _, st := range steps {
		if strings.HasPrefix(st.action, "COMPENSATE_") {
			compSteps = append(compSteps, st)
		}
	}

	if len(compSteps) > 0 {
		// 补偿进行中
		for _, st := range compSteps {
			if st.status == "FAILED" {
				if c.retryCompensate(tx, txID, st) {
					if err := tx.Commit(); err != nil {
						return err
					}
					c.publishPending(txID) // 先提交再重投，避免读到未提交状态
					return nil
				}
				c.transition(tx, txID, "PROCESSING", "FAILED")
				return tx.Commit()
			}
		}
		for _, st := range compSteps {
			if st.status != "DONE" {
				return nil // 等待补偿回执
			}
		}
		c.transition(tx, txID, "PROCESSING", "COMPENSATED")
		return tx.Commit()
	}

	hasFailed, allDone := false, true
	for _, st := range steps {
		if st.status == "FAILED" {
			hasFailed = true
		}
		if st.status == "PENDING" {
			allDone = false
		}
	}

	switch {
	case hasFailed:
		// 启动补偿：为 DONE 步骤逆序生成 COMPENSATE_*（step_no 顺接）
		maxNo := 0
		for _, st := range steps {
			if st.stepNo > maxNo {
				maxNo = st.stepNo
			}
		}
		n := maxNo
		for i := len(steps) - 1; i >= 0; i-- {
			st := steps[i]
			if st.status != "DONE" {
				continue
			}
			rev, ok := contracts.ReverseAction(st.action)
			if !ok {
				continue
			}
			n++
			payload, err := compensatePayload(st, n, rev)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`INSERT INTO tx_step_log (tx_id, step_no, dcn, action, status, payload) VALUES (?,?,?,?,'PENDING',?)`,
				txID, n, st.dcn, rev, payload); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		log.Printf("tx %s: compensating", txID)
		c.publishPending(txID)
		return nil
	case allDone:
		c.transition(tx, txID, "PROCESSING", "COMMITTED")
		return tx.Commit()
	default:
		return nil
	}
}

// retryCompensate 对失败的补偿步骤重置为 PENDING（最多 3 次）；返回 true 表示可重投。
// 调用方负责提交事务后再 publishPending。
func (c *Coordinator) retryCompensate(tx *sql.Tx, txID string, st stepRow) bool {
	key := txID + ":" + strconv.Itoa(st.stepNo)
	n, _ := c.attempts.LoadOrStore(key, 0)
	if n.(int) >= 3 {
		return false
	}
	c.attempts.Store(key, n.(int)+1)
	if _, err := tx.Exec(
		`UPDATE tx_step_log SET status = 'PENDING' WHERE tx_id = ? AND step_no = ?`,
		txID, st.stepNo); err != nil {
		return false
	}
	return true
}

// reopenCompensation 处理迟到 DONE 回执：重开事务并追加一笔反向补偿。
func (c *Coordinator) reopenCompensation(txID string, stepNo int) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE tx_log SET status = 'PROCESSING' WHERE tx_id = ? AND status = 'COMPENSATED'`, txID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // 已在重开流程中
	}
	var st stepRow
	if err := tx.QueryRow(
		`SELECT step_no, dcn, action, status, payload FROM tx_step_log WHERE tx_id = ? AND step_no = ?`,
		txID, stepNo).Scan(&st.stepNo, &st.dcn, &st.action, &st.status, &st.payload); err != nil {
		return err
	}
	rev, ok := contracts.ReverseAction(st.action)
	if !ok {
		return errors.New("cannot reverse action " + st.action)
	}
	var maxNo int
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(step_no), 0) FROM tx_step_log WHERE tx_id = ?`, txID).Scan(&maxNo); err != nil {
		return err
	}
	payload, err := compensatePayload(st, maxNo+1, rev)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO tx_step_log (tx_id, step_no, dcn, action, status, payload) VALUES (?,?,?,?,'PENDING',?)`,
		txID, maxNo+1, st.dcn, rev, payload); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("tx %s: reopened for late-receipt compensation", txID)
	c.publishPending(txID)
	return nil
}

// compensatePayload 基于原步骤 payload 生成补偿消息（金额、账户不变，动作取反）。
func compensatePayload(st stepRow, newStepNo int, rev string) (string, error) {
	var orig contracts.StepMessage
	if err := json.Unmarshal([]byte(st.payload), &orig); err != nil {
		return "", err
	}
	out, err := json.Marshal(contracts.StepMessage{
		TxID: orig.TxID, StepNo: newStepNo, Action: rev,
		AccountID: orig.AccountID, Amount: orig.Amount,
	})
	return string(out), err
}

func loadSteps(tx *sql.Tx, txID string) ([]stepRow, error) {
	rows, err := tx.Query(
		`SELECT step_no, dcn, action, status, payload FROM tx_step_log WHERE tx_id = ? ORDER BY step_no`,
		txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var st stepRow
		if err := rows.Scan(&st.stepNo, &st.dcn, &st.action, &st.status, &st.payload); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// transition 更新事务状态并打日志（verify 依赖日志可读性）。
func (c *Coordinator) transition(tx *sql.Tx, txID, from, to string) {
	if _, err := tx.Exec(`UPDATE tx_log SET status = ? WHERE tx_id = ?`, to, txID); err != nil {
		log.Printf("tx %s: transition to %s failed: %v", txID, to, err)
		return
	}
	log.Printf("tx %s: %s -> %s", txID, from, to)
}

// ---------- 超时与恢复 ----------

func (c *Coordinator) timeoutLoop() {
	t := time.NewTicker(time.Second)
	for range t.C {
		rows, err := c.db.Query(
			`SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'
			 AND created_at < NOW() - INTERVAL ? SECOND`, int(c.timeout.Seconds()))
		if err != nil {
			log.Printf("timeout scan: %v", err)
			continue
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			c.expire(id)
		}
	}
}

// expire 把超时事务的 PENDING 步骤标记 FAILED 并推进补偿；补偿进行中的事务跳过。
func (c *Coordinator) expire(txID string) {
	tx, err := c.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ? FOR UPDATE`, txID).
		Scan(&status); err != nil || status != "PROCESSING" {
		return
	}
	var comps int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tx_step_log WHERE tx_id = ? AND action LIKE 'COMPENSATE_%'`,
		txID).Scan(&comps); err != nil || comps > 0 {
		return
	}
	if _, err := tx.Exec(
		`UPDATE tx_step_log SET status = 'FAILED' WHERE tx_id = ? AND status = 'PENDING'`,
		txID); err != nil {
		return
	}
	if err := tx.Commit(); err != nil {
		return
	}
	log.Printf("tx %s: timed out, marking pending steps FAILED", txID)
	if err := c.advance(txID); err != nil {
		log.Printf("tx %s: advance after timeout: %v", txID, err)
	}
}

// recover 启动时续跑未完成事务：重发 PENDING 步骤（DCN 端幂等），
// 已在补偿中的事务同样靠重发 PENDING 补偿步骤续跑。
func (c *Coordinator) recover() {
	rows, err := c.db.Query(`SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'`)
	if err != nil {
		log.Printf("recover scan: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		log.Printf("tx %s: recovering", id)
		c.publishPending(id)
	}
}
```

- [ ] **Step 2: 编写入口 main**

`templates/dcn/cmd/rmb-coordinator/main.go`：

```go
package main

import (
	"strconv"
	"time"

	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
	"dcn/internal/rmb"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	secs, err := strconv.Atoi(runx.Env("TX_TIMEOUT_SECONDS", "5"))
	if err != nil || secs <= 0 {
		secs = 5
	}
	coord := rmb.NewCoordinator(db, mqc, time.Duration(secs)*time.Second)
	coord.Run()
	runx.Serve(":"+runx.Env("PORT", "8080"), coord.Handler())
}
```

- [ ] **Step 3: 构建**

Run: `cd templates/dcn && go build ./... && go vet ./internal/rmb/`
Expected: 构建成功、vet 无告警。

- [ ] **Step 4: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add RMB transaction coordinator with timeout compensation and recovery"
```

---

### Task 6: ADM 全局汇总服务

**Files:**
- Create: `templates/dcn/internal/adm/server.go`
- Create: `templates/dcn/cmd/adm/main.go`

**Interfaces:**
- Consumes: `contracts.BalanceEvent`（Task 2）、GNS `GET /routes`（Task 3）、DCN `GET /internal/balance-sum`（Task 4）。
- Produces: `adm.NewServer(db *sql.DB, gns string) *Server`、`(*Server).Handler() http.Handler`、`(*Server).DeclareAndConsume(mqc *mq.Conn)`。HTTP 契约：`GET /report/summary` → `{accounts,totalBalance,perDcn:[{dcn,accounts,totalBalance}]}`；`GET /reconcile` → `{consistent,admTotal,dcnTotal,perDcn:[...],errors:[...]}`。

- [ ] **Step 1: 实现 ADM 服务**

`templates/dcn/internal/adm/server.go`：

```go
// Package adm 实现 ADM 区服务：订阅各 DCN 变更事件维护全局汇总视图，
// 提供全局报表与全行余额核对（仿真生产的 T+x 汇总链路，允许秒级延迟）。
package adm

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"dcn/internal/contracts"
	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
)

// Server 是 ADM 服务。
type Server struct {
	db  *sql.DB
	gns string
	hc  *http.Client
}

// NewServer 构造 ADM 服务。
func NewServer(db *sql.DB, gns string) *Server {
	return &Server{db: db, gns: gns, hc: &http.Client{Timeout: 2 * time.Second}}
}

// Handler 返回 HTTP 路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /report/summary", s.handleSummary)
	mux.HandleFunc("GET /reconcile", s.handleReconcile)
	return mux
}

// DeclareAndConsume 声明事件拓扑并启动消费。
func (s *Server) DeclareAndConsume(mqc *mq.Conn) {
	mqc.DeclareFanoutExchange("adm.events")
	mqc.DeclareQueue("adm.events")
	mqc.Bind("adm.events", "adm.events", "")
	mqc.Consume("adm.events", s.handleEvent)
}

// handleEvent 幂等落事件并更新全局镜像（uk_event 去重）。
func (s *Server) handleEvent(body []byte) error {
	var evt contracts.BalanceEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil
	}
	amt, err := decimal.NewFromString(evt.Amount)
	if err != nil {
		return nil
	}
	delta := amt
	if evt.Direction == "DEBIT" {
		delta = amt.Neg()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO event_log (tx_id, account_id, dcn, direction, amount) VALUES (?,?,?,?,?)`,
		evt.TxID, evt.AccountID, evt.DCN, evt.Direction, amt.String()); err != nil {
		if mysqlx.IsDuplicate(err) {
			return nil // 重复事件：幂等忽略
		}
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO global_balance (account_id, dcn, balance) VALUES (?,?,?)
		 ON DUPLICATE KEY UPDATE balance = balance + VALUES(balance)`,
		evt.AccountID, evt.DCN, delta.String()); err != nil {
		return err
	}
	return tx.Commit()
}

type dcnStat struct {
	DCN      string `json:"dcn"`
	Accounts int    `json:"accounts"`
	Total    string `json:"totalBalance"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	var accounts int
	var total sql.NullString
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(balance), 0) FROM global_balance`).
		Scan(&accounts, &total); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := s.db.Query(
		`SELECT dcn, COUNT(*), COALESCE(SUM(balance), 0) FROM global_balance GROUP BY dcn ORDER BY dcn`)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	per := []dcnStat{}
	for rows.Next() {
		var st dcnStat
		if err := rows.Scan(&st.DCN, &st.Accounts, &st.Total); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		per = append(per, st)
	}
	httpx.JSON(w, 200, map[string]any{
		"accounts": accounts, "totalBalance": total.String, "perDcn": per,
	})
}

type routeView struct {
	DCN      string `json:"dcn"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// handleReconcile 对比 ADM 汇总与各 DCN 实时余额之和。
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	resp, err := s.hc.Get(s.gns + "/routes")
	if err != nil {
		httpx.Error(w, 502, "gns unreachable: "+err.Error())
		return
	}
	var routes []routeView
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		resp.Body.Close()
		httpx.Error(w, 502, "invalid gns response")
		return
	}
	resp.Body.Close()

	dcnTotal := decimal.Zero
	per := []map[string]any{}
	errs := []string{}
	for _, rt := range routes {
		if rt.Status != "ACTIVE" {
			continue
		}
		rs, err := s.hc.Get(rt.Endpoint + "/internal/balance-sum")
		if err != nil {
			errs = append(errs, rt.DCN+": "+err.Error())
			continue
		}
		var v struct {
			Accounts   int    `json:"accounts"`
			BalanceSum string `json:"balanceSum"`
		}
		if err := json.NewDecoder(rs.Body).Decode(&v); err != nil {
			rs.Body.Close()
			errs = append(errs, rt.DCN+": invalid response")
			continue
		}
		rs.Body.Close()
		sum, err := decimal.NewFromString(v.BalanceSum)
		if err != nil {
			errs = append(errs, rt.DCN+": bad balanceSum")
			continue
		}
		dcnTotal = dcnTotal.Add(sum)
		per = append(per, map[string]any{
			"dcn": rt.DCN, "accounts": v.Accounts, "balanceSum": sum.String(),
		})
	}

	var admStr sql.NullString
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(balance), 0) FROM global_balance`).Scan(&admStr); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	admTotal, _ := decimal.NewFromString(admStr.String)
	consistent := len(errs) == 0 && admTotal.Equal(dcnTotal)
	log.Printf("reconcile: adm=%s dcn=%s consistent=%v", admTotal, dcnTotal, consistent)
	httpx.JSON(w, 200, map[string]any{
		"consistent": consistent,
		"admTotal":   admTotal.String(),
		"dcnTotal":   dcnTotal.String(),
		"perDcn":     per,
		"errors":     errs,
	})
}
```

- [ ] **Step 2: 编写入口 main**

`templates/dcn/cmd/adm/main.go`：

```go
package main

import (
	"dcn/internal/adm"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	srv := adm.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	srv.DeclareAndConsume(mqc)
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
```

- [ ] **Step 3: 构建**

Run: `cd templates/dcn && go build ./...`
Expected: 构建成功。

- [ ] **Step 4: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add ADM global summary service with reconcile endpoint"
```

---

### Task 7: seed 命令（jiade seed 兼容）

**Files:**
- Create: `templates/dcn/cmd/seed/main.go`

**Interfaces:**
- Consumes: GNS `POST /accounts`（Task 3，requestId 幂等）。
- Produces: `go run ./cmd/seed --scale=dev|full [--reset]`；env 覆盖 `GNS_ENDPOINT`（默认 `http://localhost:18080`）、`SEED_DSN_{GNS,RMB,ADM,DCN01,DCN02,DCN03}`、`SEED_REDIS_ADDR`。jiade CLI 的 `seed` 子命令直接调用它。

- [ ] **Step 1: 实现 seed**

`templates/dcn/cmd/seed/main.go`：

```go
// seed 经 GNS 全流程开户灌入确定性测试数据（仿真生产的路由注册）。
// jiade CLI 硬编码调用：go run ./cmd/seed --scale=<dev|full> [--reset]
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

var (
	scale = flag.String("scale", "dev", "dev|full")
	reset = flag.Bool("reset", false, "clear all business data before seeding")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	flag.Parse()
	counts := map[string]int{"dev": 2, "full": 50}
	n, ok := counts[*scale]
	if !ok {
		log.Fatalf("unknown scale %q (want dev|full)", *scale)
	}
	if *reset {
		resetAll()
	}
	gns := envOr("GNS_ENDPOINT", "http://localhost:18080")
	hc := &http.Client{Timeout: 10 * time.Second}
	for _, seg := range []int{1000, 2000, 3000} {
		for i := 0; i < n; i++ {
			body, _ := json.Marshal(map[string]string{
				"name":        fmt.Sprintf("User-%d-%d", seg, i),
				"initBalance": "1000.00",
				"requestId":   fmt.Sprintf("seed-%s-%d-%d", *scale, seg, i), // 幂等键
			})
			resp, err := hc.Post(gns+"/accounts", "application/json", bytes.NewReader(body))
			if err != nil {
				log.Fatalf("open account via GNS: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Fatalf("GNS returned %d: %s", resp.StatusCode, raw)
			}
			fmt.Printf("seeded: %s\n", raw)
		}
	}
	fmt.Println("seed done")
}

// resetAll 清空业务数据（保留 route_segment），并清 GNS 路由缓存。
func resetAll() {
	dbs := map[string][]string{
		envOr("SEED_DSN_GNS", "root:dcn123@tcp(127.0.0.1:13309)/gns_db"):     {"account_route"},
		envOr("SEED_DSN_RMB", "root:dcn123@tcp(127.0.0.1:13310)/rmb_db"):     {"tx_step_log", "tx_log"},
		envOr("SEED_DSN_ADM", "root:dcn123@tcp(127.0.0.1:13311)/adm_db"):     {"event_log", "global_balance"},
		envOr("SEED_DSN_DCN01", "root:dcn123@tcp(127.0.0.1:13306)/dcn01_db"): {"journal", "account"},
		envOr("SEED_DSN_DCN02", "root:dcn123@tcp(127.0.0.1:13307)/dcn02_db"): {"journal", "account"},
		envOr("SEED_DSN_DCN03", "root:dcn123@tcp(127.0.0.1:13308)/dcn03_db"): {"journal", "account"},
	}
	for dsn, tables := range dbs {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			log.Fatalf("open %s: %v", dsn, err)
		}
		for _, t := range tables {
			if _, err := db.Exec("DELETE FROM " + t); err != nil {
				log.Fatalf("clear %s: %v", t, err)
			}
		}
		db.Close()
	}
	rdb := redis.NewClient(&redis.Options{Addr: envOr("SEED_REDIS_ADDR", "127.0.0.1:16379")})
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		log.Printf("warn: flush gns redis: %v", err)
	}
	fmt.Println("reset done")
}
```

- [ ] **Step 2: 构建与静态检查**

Run: `cd templates/dcn && gofmt -l . && go build ./...`
Expected: `gofmt -l` 无输出，构建成功。

- [ ] **Step 3: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add seed command compatible with jiade seed"
```

---

### Task 8: verify.sh 端到端验收（七大 gate）

**Files:**
- Create: `templates/dcn/test/verify.sh`

**Interfaces:**
- Consumes: 全部服务的 HTTP 契约（Tasks 3–6）、`docker stop/start/restart`、`docker exec dcn-rabbitmq rabbitmqadmin`、`docker compose --profile expansion`。
- Produces: `make verify` 的退出码（0 = 全部 gate 通过）；jiade CI 的 `dcn-e2e` job 调用它。

注意：脚本假设初始状态为 `make up && make seed`（dev 规模：1001/1002、2001/2002、3001/3002，各 1000.00）。

- [ ] **Step 1: 编写 verify.sh**

`templates/dcn/test/verify.sh`（可执行，`chmod +x`）：

```bash
#!/usr/bin/env bash
# DCN 架构仿真 · 端到端验收
# gate 1  DCN 内本地转账
# gate 2  跨 DCN 转账（RMB 总事务）
# gate 3  爆炸半径（dcn02-db 宕机 + 迟到回执再补偿）
# gate 4  协调者崩溃恢复（事务续跑）
# gate 5  子事务幂等（重复投递）
# gate 6  在线扩容（dcn04）
# gate 7  ADM 全局汇总与核对
set -u
cd "$(dirname "$0")/.."

GNS=${GNS:-http://localhost:18080}
DCN01=${DCN01:-http://localhost:18081}
DCN02=${DCN02:-http://localhost:18082}
DCN03=${DCN03:-http://localhost:18083}
DCN04=${DCN04:-http://localhost:18084}
RMB=${RMB:-http://localhost:18090}
ADM=${ADM:-http://localhost:18091}

FAILED=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILED=1; }

# 失败中途退出时尽量恢复环境
trap 'docker start dcn02-db >/dev/null 2>&1; docker start dcn02-app >/dev/null 2>&1; true' EXIT

balance() { curl -sf "$1/accounts/$2/balance" | jq -r '.balance'; }

# assert_delta <before> <after> <delta> <desc>：before - after == delta（数值比较）
assert_delta() {
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" --arg d "$3" \
    '((($a|tonumber) - ($b|tonumber) - ($d|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$4" || fail "$4 (before=$1 after=$2 want-delta=$3)"
}

assert_equal() { # <a> <b> <desc>：a == b（数值比较）
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" \
    '((($a|tonumber) - ($b|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$3" || fail "$3 ($1 != $2)"
}

wait_tx() { # <txId> <limit_sec> → 打印最终状态
  local txid=$1 s=""
  for _ in $(seq "$2"); do
    s=$(curl -sf "$RMB/transactions/$txid" | jq -r '.status' 2>/dev/null || true)
    [ -n "$s" ] && [ "$s" != "PROCESSING" ] && [ "$s" != "null" ] && { echo "$s"; return; }
    sleep 1
  done
  echo "$s"
}

wait_url() { # <url> <limit_sec>
  for _ in $(seq "$2"); do
    curl -sf "$1" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

echo "== Gate 1: DCN 内转账（本地事务）=="
b1=$(balance $DCN01 1001); b2=$(balance $DCN01 1002)
curl -sf -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"100.00"}' >/dev/null \
  && pass "本地转账请求成功" || fail "本地转账请求失败"
assert_delta "$b1" "$(balance $DCN01 1001)" 100 "1001 扣款 100"
assert_delta "$b2" "$(balance $DCN01 1002)" -100 "1002 入账 100"

echo "== Gate 2: 跨 DCN 转账（RMB 总事务）=="
b1=$(balance $DCN01 1001); b2=$(balance $DCN02 2001)
curl -sf -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":2001,"amount":"50.00"}' >/dev/null \
  && pass "跨 DCN 转账请求成功" || fail "跨 DCN 转账请求失败"
assert_delta "$b1" "$(balance $DCN01 1001)" 50 "1001 扣款 50"
assert_delta "$b2" "$(balance $DCN02 2001)" -50 "2001 入账 50"

echo "== Gate 3: 爆炸半径（docker stop dcn02-db）=="
pre1001=$(balance $DCN01 1001); pre2001=$(balance $DCN02 2001); pre3001=$(balance $DCN03 3001)
docker stop dcn02-db >/dev/null
# 3a. dcn01 本地交易不受影响
curl -sf -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"10.00"}' >/dev/null \
  && pass "dcn02 宕机时 dcn01 本地转账成功" || fail "dcn02 宕机影响 dcn01 本地转账"
# 3b. 涉及 dcn02 的跨单元交易：明确报错且总事务 COMPENSATED
G3TX="verify-g3-$$"
curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G3TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"20.00\"}" >/dev/null
st=$(wait_tx "$G3TX" 30)
[ "$st" = "COMPENSATED" ] && pass "故障交易被逆序补偿 (COMPENSATED)" || fail "故障交易状态=$st，期望 COMPENSATED"
want1001=$(jq -n --arg a "$pre1001" '(($a|tonumber) - 10) | tostring')
assert_equal "$want1001" "$(balance $DCN01 1001)" "1001 仅承担 3a 的本地转账，故障交易扣款已冲正"
# 3c. dcn03 不受影响
assert_equal "$pre3001" "$(balance $DCN03 3001)" "dcn03 余额不受影响"
# 3d. 恢复后：迟到的 CREDIT 回执触发再补偿，2001 回到 gate 前余额
docker start dcn02-db >/dev/null
wait_url "$DCN02/healthz" 90 && pass "dcn02 恢复" || fail "dcn02 未恢复"
ok=false
for _ in $(seq 60); do
  cur=$(balance $DCN02 2001 2>/dev/null || echo "")
  r=$(jq -n --arg a "$cur" --arg b "$pre2001" \
    '((($a|tonumber?) - ($b|tonumber)) | . * .) < 0.000001' 2>/dev/null || echo false)
  [ "$r" = "true" ] && { ok=true; break; }
  sleep 1
done
$ok && pass "迟到回执再补偿：2001 余额最终恢复" || fail "2001 余额未恢复 (=$cur, want $pre2001)"

echo "== Gate 4: 协调者崩溃恢复（事务续跑）=="
s1=$(balance $DCN01 1001); s2=$(balance $DCN02 2001)
G4TX="verify-g4-$$"
docker stop dcn02-app >/dev/null
( curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G4TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"25.00\"}" \
  >/tmp/dcn-g4-resp.json 2>&1 ) &
sleep 1
docker restart -t 1 rmb-coordinator >/dev/null
sleep 2
docker start dcn02-app >/dev/null
wait_url "$DCN02/healthz" 90 || fail "dcn02-app 未恢复"
st=$(wait_tx "$G4TX" 60)
[ "$st" = "COMMITTED" ] && pass "协调者重启后事务续跑成功 (COMMITTED)" || fail "事务状态=$st，期望 COMMITTED"
r=$(jq -n --arg a "$s1" --arg b "$s2" --arg c "$(balance $DCN01 1001)" --arg d "$(balance $DCN02 2001)" \
  '(((($a|tonumber) + ($b|tonumber)) - (($c|tonumber) + ($d|tonumber))) | . * .) < 0.000001')
[ "$r" = "true" ] && pass "两库余额合计不变" || fail "两库余额合计发生变化"

echo "== Gate 5: 子事务幂等（重复投递）=="
G5TX="verify-g5-$$"
curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G5TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"50.00\"}" >/dev/null
st=$(wait_tx "$G5TX" 30)
[ "$st" = "COMMITTED" ] || fail "gate5 前置转账未提交 (=$st)"
pre=$(balance $DCN01 1001)
docker exec dcn-rabbitmq rabbitmqadmin -u dcn -p dcn123 publish \
  exchange=rmb.steps routing_key=step.dcn01 payload_encoding=string \
  payload="{\"txId\":\"$G5TX\",\"stepNo\":1,\"action\":\"DEBIT\",\"accountId\":1001,\"amount\":\"50.00\"}" \
  >/dev/null 2>&1 && pass "重复子事务已投递" || fail "rabbitmqadmin 投递失败"
sleep 3
assert_equal "$pre" "$(balance $DCN01 1001)" "重复投递不产生重复扣款"

echo "== Gate 6: 在线扩容（dcn04）=="
docker compose --profile expansion up -d --build dcn04-db dcn04-app >/dev/null
wait_url "$DCN04/healthz" 120 && pass "dcn04 单元就绪" || fail "dcn04 未就绪"
curl -sf -X POST "$GNS/routes" -H 'Content-Type: application/json' \
  -d '{"dcn":"dcn04","segStart":4000,"segEnd":4999,"endpoint":"http://dcn04-app:8080"}' >/dev/null \
  && pass "dcn04 号段注册成功" || fail "dcn04 号段注册失败"
acct=$(curl -sf -X POST "$GNS/accounts" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Expand-1\",\"initBalance\":\"500.00\",\"requestId\":\"verify-g6-$$\"}")
newid=$(echo "$acct" | jq -r '.accountId')
[ "$newid" -ge 4000 ] 2>/dev/null && [ "$newid" -le 4999 ] 2>/dev/null \
  && pass "新开户落入 4xxx ($newid)" || fail "新开户未落入 4xxx ($acct)"
G6TX="verify-g6-$$"
curl -s -X POST "$DCN04/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G6TX\",\"fromId\":$newid,\"toId\":1001,\"amount\":\"30.00\"}" >/dev/null
st=$(wait_tx "$G6TX" 30)
[ "$st" = "COMMITTED" ] && pass "跨新旧单元转账成功 (COMMITTED)" || fail "跨新旧单元转账状态=$st"

echo "== Gate 7: ADM 全局汇总与核对 =="
sleep 3 # 容忍汇总链路秒级延迟（仿真 T+x）
sum=$(curl -sf "$ADM/report/summary")
accs=$(echo "$sum" | jq -r '.accounts')
[ "$accs" -ge 7 ] 2>/dev/null && pass "全局账户数 $accs >= 7" || fail "全局账户数异常: $sum"
rec=$(curl -sf "$ADM/reconcile")
[ "$(echo "$rec" | jq -r '.consistent')" = "true" ] \
  && pass "ADM 汇总与各 DCN 实时余额核对一致" || fail "核对不一致: $rec"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "VERIFY FAILED"
  exit 1
fi
echo "VERIFY OK"
```

- [ ] **Step 2: 全链路首跑（端到端验证 Tasks 1–8）**

Run: `cd templates/dcn && make up && make seed && make verify`
Expected: 7 个 gate 全部 PASS，末尾 `VERIFY OK`。

这是首个真实集成点。常见失败模式与对策：

- 服务起不来：`docker compose logs <svc>` 看 mysqlx/mq 重试日志；健康检查全绿前 `--wait` 会阻塞。
- Gate 3 补偿超时：确认 `TX_TIMEOUT_SECONDS=5` 且协调服务日志出现 `timed out` / `compensating` / `COMPENSATED`。
- Gate 4 时序：若 `wait_tx` 超时，加大 sleep 或确认 recover 日志 `tx ... recovering`。
- Gate 6 开户没落 4xxx：确认 `PickSegment` 选账户数最少号段（dcn04 为 0）。

- [ ] **Step 3: Commit**

```bash
git add templates/dcn
git commit -m "feat(dcn): add end-to-end verify suite covering blast radius, recovery, idempotency and expansion"
```

---

### Task 9: 模板文档（README 双语 + ARCHITECTURE）

**Files:**
- Create: `templates/dcn/README.md`
- Create: `templates/dcn/README.zh-CN.md`
- Create: `templates/dcn/ARCHITECTURE.md`

**Interfaces:** 无代码依赖；内容必须覆盖 spec §11 的五条生产差异与「特定机构名零出现」约束。

- [ ] **Step 1: 编写 README.md（英文）**

结构（每节 5–15 行，直接成文，不复述本计划代码）：

1. 标题与一句话简介：DCN 单元化架构仿真（GNS / RMB / DCN 单元 / ADM）。
2. 架构图（mermaid flowchart，与 spec §2 拓扑一致：idc1 / idc2 / global-net 三个子图，DCN 间无直连边）。
3. 组件表：DCN / GNS / RMB / ADM 职责四行。
4. 号段路由表：1000–1999→dcn01(idc1)、2000–2999→dcn02(idc2)、3000–3999→dcn03(idc1)、4000–4999→dcn04(扩容)。
5. Quickstart：`make up && make seed && make verify`；jiade 用户 `jiade init --template dcn --dir ./mydcn && cd ./mydcn && jiade up && jiade seed`。
6. 手动体验命令：本地转账、跨 DCN 转账、`curl localhost:18091/report/summary`、扩容四步（`docker compose --profile expansion up -d --build dcn04-db dcn04-app` → `POST /routes` → 开户 → 跨单元转账）。
7. 端口速查表（18080–18091 / 13306–13312 / 15672 / 16379）。
8. **与生产架构的差异**（必须包含这五条，逐条成文）：
   - 生产每 DCN 客户数百万级、库为一主两从的分布式数据库；仿真为单实例 MySQL 8、每 DCN 千级号段。
   - 生产 RMB 为自研总线，具备流控/熔断/权限管控；仿真用 RabbitMQ + 自写协调服务，仅实现核心事务语义。
   - 生产多 IDC 同城多副本 + 异地灾备；仿真以 docker network 模拟 2 个 IDC 的主库交叉部署，不实现副本；同 IDC 内多 DCN 共享一个 network，网络层隔离不仿真（跨单元不直连靠应用约束）。
   - 生产全局场景（存证、批量）使用原生分布式数据库；仿真以 MySQL 代替。
   - 仿真不含安全合规能力（加密、审计、权限），仅供架构学习演示。
9. 已知简化：ADM 汇总为秒级延迟（仿真 T+x 链路）；补偿重试 3 次后转 FAILED 需人工介入；seed --reset 仅覆盖基础三单元拓扑。

- [ ] **Step 2: 编写 README.zh-CN.md**

与 README.md 同构的中文版，章节一一对应；Quickstart 与命令保持原文。

- [ ] **Step 3: 编写 ARCHITECTURE.md**

深度设计文档，章节：

1. 设计动机与单元化收益（水平扩展 / 爆炸半径 / 单实例库即可 / 主库交叉部署）。
2. 四大组件职责与关键设计规则六条（交易本地化、跨单元必经 RMB、RMB 协调跨单元事务、主库交叉、一主两从（生产）/单实例（仿真）、全局场景归 ADM）。
3. 关键流程时序（mermaid sequenceDiagram）：DCN 内转账、跨 DCN 转账（含失败补偿与迟到回执再补偿）、ADM 汇总链路。
4. 事务协调状态机：PROCESSING / COMMITTED / COMPENSATED / FAILED 迁移图与触发条件（回执齐、步骤失败、超时器 5s、补偿失败 3 次、崩溃恢复扫描）。
5. 幂等设计清单：journal `uk_tx_acct`、补偿 `:comp` 后缀、GNS requestId、ADM `uk_event`、协调服务 txId 幂等注册。
6. 数据模型（四库 DDL 摘要）。
7. 故障与扩容用例表（verify 七 gate 与预期）。

- [ ] **Step 4: 命名约束检查**

Run: `grep -ri "$(printf '\xe5\xbe\xae\xe4\xbc\x97')\|$(printf 'web')ank" templates/dcn/ ; echo "exit=$?"`
Expected: 无匹配，`exit=1`（grep 无结果）。

- [ ] **Step 5: Commit**

```bash
git add templates/dcn
git commit -m "docs(dcn): add bilingual README and architecture doc"
```

---

### Task 10: jiade 接入与全量收尾

**Files:**
- Modify: `internal/template/pack.go:48-51`
- Modify: `Makefile`（根）
- Modify: `.github/workflows/ci.yml`
- Modify: `README.md:15-16`（模板表）、`README.zh-CN.md`（对应表）
- Regenerate: `internal/template/templates.tar`

**Interfaces:**
- Consumes: `templates/dcn` 全部产物（Tasks 1–9）。
- Produces: `jiade list` 输出含 `dcn`；`jiade init --template dcn` 可用；CI 新增 `dcn` / `dcn-e2e` job。

- [ ] **Step 1: 放行 dcn 目录进 tar 白名单**

修改 `internal/template/pack.go:48-51`：

```go
		// Only pack the bank, commerce and dcn subdirectories.
		if !strings.HasPrefix(rel, "bank/") && !strings.HasPrefix(rel, "commerce/") &&
			!strings.HasPrefix(rel, "dcn/") {
			return nil
		}
```

同步把文件头注释 `from the templates/bank and templates/commerce directories` 改为 `from the templates/bank, templates/commerce and templates/dcn directories`。

- [ ] **Step 2: 重新打包并跑 jiade 侧测试**

Run: `go generate ./internal/template && go test ./internal/template/...`
Expected: `packed N files into templates.tar`；`TestTemplateArchiveMatchesTemplateSources` 等全部 PASS。

- [ ] **Step 3: 根 Makefile 增加 dcn-ci**

在 `commerce-ci` 目标之后追加：

```make
# dcn template static verification (build, test, compose topology)
dcn-ci:
	cd templates/dcn && go build ./...
	cd templates/dcn && go test ./...
	cd templates/dcn && $(MAKE) topology-test
```

并把第 1 行 `.PHONY` 列表加入 `dcn-ci`。

- [ ] **Step 4: CI 增加 dcn / dcn-e2e job**

在 `.github/workflows/ci.yml` 的 `commerce-e2e` job 之后追加（镜像 `bank` job 的结构）：

```yaml
  dcn:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: cd templates/dcn && go build ./...
      - run: cd templates/dcn && go test ./...
      - run: cd templates/dcn && make topology-test

  dcn-e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: Start dcn
        run: cd templates/dcn && make up && make seed
      - name: Verify
        run: cd templates/dcn && make verify
      - name: Capture logs
        if: failure()
        run: cd templates/dcn && docker compose logs --no-color
      - name: Cleanup
        if: always()
        run: cd templates/dcn && docker compose --profile expansion down --volumes --remove-orphans
```

- [ ] **Step 5: 根 README 更新**

`README.md` 模板表（第 15–16 行区域）在 `commerce` 行后追加一行：

```markdown
| `dcn` | DCN unit-architecture simulation — GNS global routing (Redis+MySQL), RMB reliable message bus with transaction coordinator (RabbitMQ), 3 self-contained DCN units across 2 simulated IDCs, ADM global summary & reconcile, 7-gate verify suite (blast radius, crash recovery, idempotency, online expansion). | [templates/dcn/README.md](templates/dcn/README.md) · [ARCHITECTURE.md](templates/dcn/ARCHITECTURE.md) |
```

并把第 19 行 `jiade init --template <bank|commerce>` 改为 `jiade init --template <bank|commerce|dcn>`，第 79–80 行 repo-layout 区追加一行：

```markdown
templates/dcn/       the dcn template — a standalone Go module (`module dcn`)
```

`README.zh-CN.md` 做对应更新（dcn 行用中文描述：DCN 单元化架构仿真——GNS 全局路由、RMB 可靠消息总线与事务协调、跨 2 个仿真 IDC 的 3 个自包含 DCN 单元、ADM 全局汇总核对、7 项验收用例）。

- [ ] **Step 6: jiade 全链路验收（生成工程→启动→verify）**

Run:

```bash
make test                       # jiade 构建 + 全部单测（含 tar 一致性）
rm -rf /tmp/jiade-dcn-e2e
go run ./cmd/jiade list         # 输出应包含 dcn
go run ./cmd/jiade init --template dcn --dir /tmp/jiade-dcn-e2e --force
cd /tmp/jiade-dcn-e2e && make up && make seed && make verify
```

Expected: `jiade list` 含 `dcn`；拷贝出的工程 `VERIFY OK`。

- [ ] **Step 7: 命名约束最终扫描**

Run: `grep -ri "$(printf '\xe5\xbe\xae\xe4\xbc\x97')\|$(printf 'web')ank" templates/dcn/ docs/superpowers/specs/2026-08-02-dcn-template-design.md docs/superpowers/plans/2026-08-02-dcn-template.md internal/template/pack.go README.md README.zh-CN.md .github/workflows/ci.yml ; echo "exit=$?"`
Expected: 无匹配，`exit=1`。

- [ ] **Step 8: Commit**

```bash
git add internal/template/pack.go internal/template/templates.tar Makefile .github/workflows/ci.yml README.md README.zh-CN.md
git commit -m "feat(jiade): wire dcn template into packer, CI and docs"
```
