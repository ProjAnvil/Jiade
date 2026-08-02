# DCN 架构模板 · 设计文档

> 为 jiade 新增第三个内置模板 `dcn`：DCN 单元化架构仿真版。本文档是实现依据（spec）。
> 命名约束：任何文件（spec、plan、代码、README、注释）中不得出现特定银行机构名称；架构统一称为「DCN 架构」。

## 1. 背景与目标

jiade 目前内置 `bank`、`commerce` 两个模板（`templates/<name>/`，经 `internal/template/pack.go` 白名单打入 `templates.tar`，`jiade init` 解压拷贝）。本设计新增 `templates/dcn/`，仿真 DCN（Data Center Node）单元化架构：

- 按客户号段把系统切成 N 个自包含单元（接入 + 应用 + 单实例库）；
- GNS 负责「客户 → DCN」全局路由定位；
- RMB 是 DCN 间唯一合法通信通道，兼任跨 DCN 分布式事务协调者；
- ADM 区承接无法按客户拆分的全局汇总/报表/核对；
- docker compose 用多个 network 仿真多 IDC 与主库交叉部署；
- verify 脚本验证爆炸半径、跨 DCN 一致性、幂等、在线扩容四个核心用例。

## 2. 方案取舍（已决策）

| 候选 | 说明 | 结论 |
| ---- | ---- | ---- |
| A. Go 单模块多二进制 | 对齐 bank/commerce：`module dcn`，`cmd/<svc>` 每服务一个二进制，单 Dockerfile `ARG SERVICE`，compose 从源码构建 | **采用**。CLI `jiade seed` 硬编码 `go run ./cmd/seed --scale=...`，CI/Makefile/tar 打包模式全部可复用 |
| B. Python/Node 实现 | 代码量更少 | 否决：偏离仓库全部工程惯例（Go module、CI、测试体系） |
| C. 单二进制多角色 | 一个 binary 以 flag 切换角色 | 否决：偏离「一服务一 cmd」惯例，compose 配置反而更绕 |

其他关键决策：

- **建表方式**：用 MySQL 官方镜像的 `/docker-entrypoint-initdb.d` 挂载初始化 SQL（bank 用独立 migrate 容器，dcn 模板教学定位，initdb 更简单，且扩容用例中 dcn04 首次启动自动建表）。
- **MQ 拓扑声明**：各服务启动时幂等声明自己需要的 exchange/queue，不维护 `definitions.json`。
- **端口发布**：DCN 架构要求「客户端任意入口」，故发布各应用端口与 MySQL 端口到 localhost（教学场景可直接连库观察；verify 断言统一用 `docker exec` 不依赖宿主机客户端）。这与 bank「仅 Traefik 发布端口」不同，是架构差异使然。

## 3. 模板目录结构

```
templates/dcn/
├── template.yaml            # jiade 清单（name: dcn）
├── go.mod / go.sum          # module dcn（独立模块，故随 tar 嵌入）
├── Dockerfile               # 多阶段，ARG SERVICE，编译 ./cmd/${SERVICE}
├── .dockerignore / .gitignore / .env.example
├── Makefile                 # up / down / seed / verify / topology-test / smoke
├── README.md / README.zh-CN.md / ARCHITECTURE.md
├── compose.yaml             # 主拓扑（含 dcn04 扩容 profile）
├── cmd/
│   ├── gns/main.go
│   ├── rmb-coordinator/main.go
│   ├── dcn-app/main.go      # dcn01/02/03/04 同构，env 区分
│   ├── adm/main.go
│   └── seed/main.go         # --scale=dev|full --reset（jiade seed 兼容）
├── internal/
│   ├── platform/            # mysqlx, redisx, mq(amqp), httpx, runx, ratelimit
│   ├── gns/                 # 路由定位、开户、号段管理
│   ├── rmb/                 # 总事务注册/分发/回执/超时补偿/崩溃恢复
│   ├── dcnapp/              # 账户业务、子事务执行、ADM 事件上报
│   └── adm/                 # 事件消费、全局汇总、核对
├── db/init/
│   ├── gns/01_init.sql
│   ├── rmb/01_init.sql
│   ├── dcn/01_init.sql      # 四个 DCN 库共用
│   └── adm/01_init.sql
└── test/
    ├── verify.sh            # 核心验收（见 §8）
    └── topology.sh          # compose config + jq 静态拓扑断言
```

Go 依赖（最小集）：`github.com/go-sql-driver/mysql`、`github.com/redis/go-redis/v9`、`github.com/rabbitmq/amqp091-go`、`gopkg.in/yaml.v3`（仅如需）。HTTP 用标准库 `net/http`（Go 1.22 mux 带路径参数）。

## 4. 拓扑与网络

三个 docker network 仿真两个 IDC 与全局区：

| network | 容器 |
| ------- | ---- |
| `idc1` | dcn01-app, dcn01-db, dcn03-app, dcn03-db |
| `idc2` | dcn02-app, dcn02-db, (dcn04-app, dcn04-db — profile `expansion`) |
| `global-net` | gns, gns-db, gns-redis, rabbitmq, rmb-coordinator, rmb-db, adm, adm-db |

规则：

- DCN 应用容器**双网卡**：`dcnXX-app` 同时加入 `idcN`（访问本单元 DB）与 `global-net`（访问 GNS / RabbitMQ / RMB）。
- DCN 数据库**只**在所属 `idcN`，全局区无法直连任何 DCN 库。
- DCN 应用之间**无应用层直连**：跨单元只走 RMB。仿真局限：同 IDC 的 DCN（dcn01/dcn03）共享一个 network，网络层互可达，隔离靠「代码不直连 + 文档约束」保证——此差异写入 README 第 8 节。
- 主库交叉部署：dcn01→idc1、dcn02→idc2、dcn03→idc1、dcn04→idc2（轮换）。

号段规划（GNS 初始路由表）：

| 号段 | DCN | 主库 IDC |
| ---- | --- | -------- |
| 1000–1999 | dcn01 | idc1 |
| 2000–2999 | dcn02 | idc2 |
| 3000–3999 | dcn03 | idc1 |
| 4000–4999 | dcn04 | idc2（扩容用例注册，初始不存在） |

宿主机端口发布：

| 端口 | 服务 | 端口 | 服务 |
| ---- | ---- | ---- | ---- |
| 18080 | gns | 13306 | dcn01-db |
| 18081 | dcn01-app | 13307 | dcn02-db |
| 18082 | dcn02-app | 13308 | dcn03-db |
| 18083 | dcn03-app | 13309 | gns-db |
| 18084 | dcn04-app（expansion） | 13310 | rmb-db |
| 18090 | rmb-coordinator | 13311 | adm-db |
| 18091 | adm | 13312 | dcn04-db（expansion） |
| 15672 | rabbitmq 管理台 | 16379 | gns-redis（seed --reset 清缓存用） |

基础设施约定：rabbitmq 镜像固定 `rabbitmq:3.13-management`，以 `RABBITMQ_DEFAULT_USER/PASS` 建专用账号（guest 默认仅允许 localhost 连接，跨容器不可用）；MySQL 统一 `MYSQL_ROOT_HOST=%`，root 密码经 `.env.example` 的 `MYSQL_ROOT_PASSWORD` 注入（默认 `dcn123`）。

## 5. 消息拓扑（RabbitMQ）

- exchange `rmb.steps`（topic，durable）：routing key `step.<dcn>`，每 DCN 一个 durable 队列 `rmb.steps.<dcn>`，由 rmb-coordinator 声明。
- queue `rmb.receipts`（durable）：各 DCN 应用回执，rmb-coordinator 消费。
- exchange `adm.events`（fanout，durable）→ queue `adm.events`：DCN 应用发布账户变更事件，ADM 消费。
- 所有消息 `delivery_mode=2`（持久化）；消费者手动 ack，处理成功才 ack。

## 6. 数据模型

gns_db：

```sql
CREATE TABLE route_segment (
  dcn        VARCHAR(16) PRIMARY KEY,
  seg_start  INT NOT NULL,
  seg_end    INT NOT NULL,
  endpoint   VARCHAR(128) NOT NULL,   -- http://dcn01-app:8080
  status     VARCHAR(16) NOT NULL     -- ACTIVE / DRAINING
);
CREATE TABLE account_route (
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  request_id VARCHAR(64) UNIQUE,      -- 开户幂等键，可空
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

rmb_db：

```sql
CREATE TABLE tx_log (
  tx_id      VARCHAR(64) PRIMARY KEY,
  type       VARCHAR(32) NOT NULL,
  status     VARCHAR(16) NOT NULL,    -- PROCESSING/COMMITTED/COMPENSATED/FAILED
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);
CREATE TABLE tx_step_log (
  tx_id   VARCHAR(64) NOT NULL,
  step_no INT NOT NULL,
  dcn     VARCHAR(16) NOT NULL,
  action  VARCHAR(32) NOT NULL,       -- DEBIT/CREDIT/COMPENSATE_DEBIT/COMPENSATE_CREDIT
  status  VARCHAR(16) NOT NULL,       -- PENDING/DONE/FAILED/COMPENSATED
  payload TEXT NOT NULL,              -- 原始子事务消息 JSON（补偿取参、重发、崩溃恢复的依据）
  PRIMARY KEY (tx_id, step_no)
);
```

每个 dcnXX_db：

```sql
CREATE TABLE account (
  account_id INT PRIMARY KEY,
  name       VARCHAR(64),
  balance    DECIMAL(18,2) NOT NULL CHECK (balance >= 0)
);
CREATE TABLE journal (
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  direction  VARCHAR(8) NOT NULL,     -- DEBIT/CREDIT
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_tx_acct (tx_id, account_id, direction)
);
```

adm_db：

```sql
CREATE TABLE event_log (              -- 幂等去重 + 审计
  id         BIGINT AUTO_INCREMENT PRIMARY KEY,
  tx_id      VARCHAR(64) NOT NULL,
  account_id INT NOT NULL,
  dcn        VARCHAR(16) NOT NULL,
  direction  VARCHAR(8) NOT NULL,
  amount     DECIMAL(18,2) NOT NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_event (tx_id, account_id, direction)
);
CREATE TABLE global_balance (         -- 全局镜像汇总
  account_id INT PRIMARY KEY,
  dcn        VARCHAR(16) NOT NULL,
  balance    DECIMAL(18,2) NOT NULL
);
```

## 7. 服务规格

### 7.1 GNS（cmd/gns，internal/gns）

| 接口 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/locate?accountId=` | GET | `{accountId, dcn, endpoint}`；先查 Redis（key `route:{id}`），miss 回源 MySQL 并回填；不存在返回 404 |
| `/accounts` | POST | `{name, initBalance, requestId?}`：选账户数最少的 ACTIVE 号段，分配段内下一个账号（事务 + 唯一键冲突重试），写路由表，调用目标 DCN `POST /accounts` 建户，回填缓存。`requestId` 重复 → 返回首次结果（幂等） |
| `/routes` | GET | 全量号段表 |
| `/routes` | POST | `{dcn, segStart, segEnd, endpoint}`：新增号段（dcn 主键冲突 → 幂等返回已存在）；新号段写入后无需重启任何应用 |

行为要求：`/locate` Redis 命中 P99 < 20ms；`/routes` 变更后主动删除相关缓存键。

### 7.2 RMB 协调服务（cmd/rmb-coordinator，internal/rmb）

| 接口 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/transactions` | POST | `{type:"TRANSFER", steps:[{dcn,action,params},...], txId?}`：落库 PROCESSING + 步骤 PENDING，持久化发布全部子事务消息，**同步等待**至终态或超时（默认 10s）后返回 `{txId, status}`。`txId` 已存在 → 幂等返回当前状态 |
| `/transactions/{txId}` | GET | 查询总事务与各步骤状态 |

内部机制：

- 子事务并发分发：注册时全部步骤一次性发布到各自 DCN 队列，不设步骤间顺序依赖（转账场景的 DEBIT/CREDIT 无先后约束，一致性由补偿兜底）。
- 回执消费者：更新 `tx_step_log`；全部 DONE → `COMMITTED`；任一 FAILED → 触发补偿。
- 补偿：对已成功步骤按**逆序**向对应 DCN 发送反向动作（DEBIT→COMPENSATE_DEBIT，CREDIT→COMPENSATE_CREDIT），补偿消息的 `txId` 统一加后缀 `<txId>:comp`（与原始子事务共用 tx_step_log 事务记录，但 DCN 端 journal 幂等键互不冲突），全部补偿完成 → `COMPENSATED`；补偿失败重试后仍失败 → `FAILED`。
- 超时器：每 1s 扫描 PROCESSING 且超时（默认 5s，env 可调）的事务 → 把 PENDING 步骤标记 FAILED 后进入补偿；已进入补偿（存在 COMPENSATE_* 步骤）的事务跳过。
- 崩溃恢复：启动时扫描 PROCESSING 事务，把 PENDING 步骤按 `payload` 原样重发（DCN 端幂等保证安全），随后回到正常回执/超时流程。
- 迟到回执再补偿：若某步骤已被判 FAILED（如超时）且总事务已 COMPENSATED，之后又收到该步骤的 DONE 回执（下游恢复后补执行），必须为该步骤补发一笔 COMPENSATE_* 并重新走一轮补偿，保证「余额合计不变」最终成立。
- 全部状态流转打印结构化日志（`tx_id, from_status, to_status`），供 verify 断言。

### 7.3 DCN 应用（cmd/dcn-app，internal/dcnapp；dcn01/02/03/04 同构）

env：`DCN_ID`、`DB_DSN`、`GNS_ENDPOINT`、`RMB_ENDPOINT`、`AMQP_URL`。

| 接口 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/accounts` | POST | `{accountId, name, initBalance}` 建户（GNS 调用）；account_id 主键冲突 → 幂等返回已有 |
| `/accounts/{id}/balance` | GET | 查余额 |
| `/internal/balance-sum` | GET | 本单元账户数与余额合计；ADM 经 `global-net` 调用（DCN 应用双网卡），用于 `/reconcile` 核对 |
| `/transfer` | POST | `{fromId, toId, amount}`：查 GNS 定位双方；同 DCN → 本地事务；跨 DCN → 提交 RMB 总事务并同步等待结果 |

- 本地转账：单事务内 `UPDATE account SET balance=balance-? WHERE account_id=? AND balance>=?`（条件更新防透支）+ 对方入账 + 双方 journal，任一失败整体回滚。
- 子事务消费者：消费 `rmb.steps.<dcn>`，执行 DEBIT/CREDIT/COMPENSATE_*，本地事务 + journal 唯一键幂等（重复投递 → 识别已存在直接回成功回执），回执发 `rmb.receipts`。
- 建户时若 `initBalance > 0`，同步发一笔 ADM 事件（`txId = "init-<accountId>"`，direction CREDIT），保证 ADM 全局镜像包含初始余额。
- 每笔余额变更向 `adm.events` 发事件 `{txId, accountId, dcn, direction, amount}`（at-least-once，ADM 端去重）。
- 任意入口语义：收到的 `/transfer` 若源账户不属于本单元，由接入层把请求**透明转发**到源账户所属单元的 `/transfer`（这是接入层路由，等同生产网关按 GNS 结果转发，不属于跨 DCN 业务通信）。
- 所有服务提供 `GET /healthz`（200 OK），供 compose healthcheck 与 verify 等待。
- 接入层职责以中间件实现：每实例令牌桶限流（默认 200 rps，env 可调）+ 请求日志。

### 7.4 ADM（cmd/adm，internal/adm）

| 接口 | 方法 | 说明 |
| ---- | ---- | ---- |
| `/report/summary` | GET | 全局账户数、总余额、各 DCN 分布（基于 global_balance） |
| `/reconcile` | GET | 汇总库总余额 vs 各 DCN 实时余额之和的核对结果：经 GNS `/routes` 取 ACTIVE 单元，调用各 DCN `/internal/balance-sum` 比对，返回 `{consistent, admTotal, dcnTotal, perDcn}` |

- 事件消费者：消费 `adm.events`，以 `tx_id+account_id+direction` 唯一键去重，落 event_log 并更新 global_balance。
- 允许秒级汇总延迟——README 注明「仿真生产的 T+x 汇总链路」。

### 7.5 seed（cmd/seed）

`jiade seed` 硬编码 `go run ./cmd/seed --scale=<dev|full> [--reset]`，必须兼容：

- 默认 `dev`：经 GNS `/accounts` 在每号段开 2 个账户（如 1001/1002、2001/2002、3001/3002），初始余额确定性（固定算法，非随机）。
- `full`：每号段 50 个账户。
- `--reset`：直连 localhost 各 MySQL 发布端口清空 account/journal/event_log/global_balance/account_route/tx_log/tx_step_log（route_segment 保留），并对 localhost:16379 的 gns-redis 执行 FLUSHDB（避免路由缓存脏数据），再重新灌数。
- 幂等：重复执行不产生重复账户（GNS requestId 幂等键 = `seed-<scale>-<n>`）。

## 8. verify 用例（test/verify.sh，gate 风格对齐 bank smoke.sh）

| # | 用例 | 操作 | 断言 |
| - | ---- | ---- | ---- |
| 1 | DCN 内转账 | 1001→1002 转 100 | 成功；双方余额精确变化 |
| 2 | 跨 DCN 转账 | 1001→2001 转 50 | 状态 COMMITTED；两库余额精确变化 |
| 3 | 爆炸半径 | `docker stop dcn02-db` | 1001→1002 成功；1001→2001 明确报错且总事务 COMPENSATED（1001 余额不变，扣款已被逆序冲正）；3001 余额正常；最后 `docker start dcn02-db` 恢复 |
| 4 | 协调者崩溃恢复 | 发起 1001→2001 转账期间 `docker restart rmb-coordinator` | 恢复后事务达到终态；两库余额合计与转账前一致 |
| 5 | 幂等 | 用 `docker exec dcn-rabbitmq rabbitmqadmin publish` 向 `rmb.steps.dcn01` 重投一条已完成的 DEBIT 子事务消息（rabbitmq 镜像固定为 `rabbitmq:3.13-management`，自带 rabbitmqadmin） | 余额无重复变动（journal 唯一键兜底） |
| 6 | 扩容 | `docker compose --profile expansion up -d --build dcn04-db dcn04-app` → `POST /routes` 注册 dcn04（4000–4999）（先起单元再注册路由，避免 GNS 把开户路由到未就绪单元） | 新开户落入 4xxx；4001→1001 跨新旧单元转账 COMMITTED |
| 7 | ADM 汇总 | 等待 3s 汇总延迟 | `/report/summary` 账户数/总余额正确；`/reconcile` 返回 consistent=true |

辅助：`test/topology.sh` 用 `docker compose config --format json` + jq 静态断言三网络存在、各 DB 仅接入所属 IDC 网络、DCN 应用双网卡、dcn04 在 expansion profile。

任一 gate 失败脚本整体退出 1。

## 9. jiade 侧接入改动

1. `internal/template/pack.go` 白名单加 `"dcn/"` 前缀。
2. `go generate ./internal/template` 重新生成并提交 `internal/template/templates.tar`（`TestTemplateArchiveMatchesTemplateSources` 自动覆盖 dcn）。
3. 根 `Makefile` 增加 `dcn-ci` 目标（对齐 `bank-ci`/`commerce-ci`）。
4. `.github/workflows/ci.yml` 增加 `dcn` 与 `dcn-e2e` job（`make up` → `make verify`）。
5. 根 `README.md` / `README.zh-CN.md` 的模板表与目录说明加入 dcn。

无需改动：`internal/cli`（init/list/up/down/seed 全部模板无关）；`internal/template/manifest.go`（dcn 的 template.yaml 只用现有 schema 字段）。

## 10. template.yaml

```yaml
name: dcn
description: DCN 单元化架构仿真：GNS 路由 + RMB 可靠消息总线 + 多 DCN 单元 + ADM 全局汇总
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

## 11. 与生产架构的差异（写入模板 README 第 8 节）

1. 生产每 DCN 客户数百万级、库为一主两从的分布式数据库；仿真为单实例 MySQL 8、每 DCN 千级号段。
2. 生产 RMB 为自研总线，具备流控/熔断/权限管控；仿真用 RabbitMQ + 自写协调服务，仅实现核心事务语义。
3. 生产多 IDC 同城多副本 + 异地灾备；仿真以 docker network 模拟 2 个 IDC 的主库交叉部署，不实现副本；同 IDC 内多 DCN 共享一个 network，网络层隔离不仿真。
4. 生产全局场景（存证、批量）使用原生分布式数据库；仿真以 MySQL 代替并在 ADM 文档中说明。
5. 仿真不含安全合规能力（加密、审计、权限），仅供架构学习演示。

## 12. 非目标（YAGNI）

- 不实现主从复制、多副本、异地灾备（仅文档说明）。
- 不实现 gRPC、链路追踪、可观测性叠加层（bank 已覆盖，dcn 聚焦单元化与分布式事务）。
- 不做 K8s 部署清单。
- 不做认证、加密、审计。
- 限流仅保留最简单的每实例令牌桶。
