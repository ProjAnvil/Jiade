# bank（jiade 模板：7 服务纵切——core-banking + customer + payment + reward + risk + loan + wealth）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行（仅需 docker + go）。

本模板属于 [jiade](../../README.md) 项目；架构细节见 [ARCHITECTURE.md](ARCHITECTURE.md)。

工程包含 **7 服务 + 7 独立 PostgreSQL 库 + Traefik 网关 + RabbitMQ + 内部 gRPC + 逐日滚存/三因子 fixture + durable 支付 saga + 全栈可观测性 + dev/prod Kubernetes overlay**。每个服务只访问自己的数据库：

| 服务 | 容器端口 | 库 | 默认副本 | 内容 |
|------|----------|----|----------|------|
| core-banking | 8080 (REST) / 9090 (gRPC) | core_db | 2 | 活期/定存账户、复式记账总账、逐日余额、写接口（过账/冲正）、**资金冻结（hold/release/capture）** |
| customer | 8080 (REST) / 9090 (gRPC) | cust_db | 1 | 客户信息、账户关系 |
| payment | 8080 (REST) / 9091 (admin gRPC) | pay_db | 2 | 商户、消费流水、**durable 支付工作流引擎（saga 编排 + 不可变审计）** |
| reward | 8080 (REST) | reward_db | 1 | 积分账户/流水、优惠券、活动 |
| risk | 8080 (REST) | risk_db | 2 | 风控规则、事件、黑名单、**支付授权（authorize/void）** |
| loan | 8080 (REST) | loan_db | 1 | 借据、放款、月度还款、五级分类逾期、**逐日余额快照** |
| wealth | 8080 (REST) | wealth_db | 1 | 理财产品、**逐日净值游走**、持仓、申赎订单、每日利息 |

**网关**：Traefik 是唯一对外发布的端口——`http://localhost:18000`。所有公开 REST 路径（`/api/v1/...`）经网关路由到对应服务的 8080 端口；`/internal/*`、gRPC 9090（读取）与 admin gRPC 9091（运营）**不暴露给宿主机**。

**服务间通信**：同步只读查询走内部 gRPC（customer、core-banking 在 :9090 提供 `CustomerQueryService` / `AccountQueryService`）；支付 saga 的异步命令与领域事件走 RabbitMQ（事务发件箱 + 有限重试 + DLQ + 运营者冲正）。详见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 数据引擎要点

每个服务都是同一个四层纵切（`api → service → repo → domain`）。数据引擎要点：

- **确定性 fixture**：同 seed + scale → 完全相同的行。确定性 ID（无 UUID），逐日独立 rng（`seed + 偏移 + 日序`）。
- **两种数据形态**：三因子事件流（`趋势 × 季节 × 周期`——周末单量 < 工作日）与路径依赖的**逐日滚存快照**（账户余额、借据余额、净值游走）。
- **数据库按服务隔离**：每个服务独占一个 PostgreSQL 实例与卷，只查自己的库；跨域只读数据通过内部 gRPC 获取（如 loan 调 customer 的 `CustomerQueryService`）。
- **金额 int64 分，禁 float**；利率/净值/份额等非货币小数按 NUMERIC 文本直存。
- **生成物自包含**：离开 jiade 也能构建运行——只需 Docker 和 Go。

## 快速开始

```bash
make up       # docker compose up -d --build --wait，然后 make seed
make seed     # 建 7 库 → 建 7 库表 → 灌 7 域 fixture（9 步，幂等：--reset）
```

灌数规模：`--scale=dev`（约 1/4 量，默认）或 `--scale=full`。同 seed 重跑 `make seed`（或 `jiade seed`）产出完全相同的数据。`make seed` 走 `--reset`，会重建全部 7 库。

```bash
make seed                       # dev 规模（默认）
SCALE=full make seed            # full 规模
go test -tags=integration -p 1 ./...   # 集成测试，需本机 15432 有 postgres（DB_PORT 可覆盖）
```

所有公开 REST 端点经网关 `http://localhost:18000` 访问；服务容器端口不发布到宿主机。`make up` 使用 `--wait`，会等到全部 healthcheck 就绪再返回。查看单个服务健康状态：

```bash
docker compose ps                                        # 各服务 health 列
docker compose exec core-banking wget -qO- :8080/healthz # 容器内探针
```

core-banking 只读查询（Spec A，经网关）：

```bash
curl -sf localhost:18000/api/v1/accounts/D0000000001
curl -sf localhost:18000/api/v1/accounts/D0000000001/balance
```

core-banking 记账/冲正写接口（Spec B-3；复式过账强制 sum(借)==sum(贷)，`LedgerService.Post` 已内部化，客户端只见业务意图）：

```bash
# 记账：存入 100 元（deposit / withdraw / transfer）
curl -sf -X POST localhost:18000/api/v1/txns \
  -H 'Content-Type: application/json' \
  -d '{"action":"deposit","account_no":"D0000000001","amount":"100.00","ccy":"CNY"}'
# → 201 {"voucher_no":"V...","biz_date":"...","txns":[{借/贷两条分录}]}

# 冲正：蓝冲（默认，改状态+回滚余额，不新增流水）
curl -sf -X POST 'localhost:18000/api/v1/vouchers/V.../reverse?mode=blue'
# → 200 {"voucher_no":"V...","mode":"blue","status":"reversed"}
# mode=red 走反向分录（新增反向流水，返回 reversed_voucher_no）
```

## 支付工作流（durable saga）

**入口**：`POST /api/v1/payments/workflows`（payment 服务，经网关 18000 → payment:8080）。

工作流是 payment 服务内置的 durable saga 编排器（`internal/platform/workflow`）：每次请求落库为一个不可变 Instance，按顺序执行三个 Action；任一 Action 终态失败时按**逆序**触发补偿；运营者可经 admin gRPC 干预卡住的补偿。所有 saga 命令/事件经 RabbitMQ 投递，落库前与业务写在同一 PostgreSQL 事务中（事务发件箱），保证「状态变更」与「事件发出」原子一致。

### 提交一个支付

```bash
# Idempotency-Key 必填；同 key + 同 body 重放返回原 workflow_id（200, replayed=true）；
# 同 key + 不同 body 返回 409 idempotency_conflict。
curl -sf -X POST localhost:18000/api/v1/payments/workflows \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: my-key-0001' \
  -d '{
        "payer_customer_id":"C0000001",
        "payer_account_no":"A000000001",
        "payee_account_no":"A000000002",
        "currency":"CNY",
        "amount_minor":5000
      }'
# → 201 {"workflow_id":"wf-...","status":"preparing","replayed":false}
#   amount_minor 是 int64 分（5000 = 50.00 CNY）；必须 > 0。
```

查询状态：

```bash
curl -sf localhost:18000/api/v1/payments/workflows/wf-...
# → 200 {"workflow_id":"wf-...","status":"succeeded","reversed":false,...}
```

对已 `succeeded` 的工作流发起冲正（触发补偿 saga）：

```bash
curl -sf -X POST localhost:18000/api/v1/payments/workflows/wf-.../reverse
# → 200 {"workflow_id":"wf-...","reversal_workflow_id":"wf-...","status":"compensating"}
```

### 工作流状态机（Instance.Status）

```
preparing ──► ready ──► running ──┬─► succeeded
                                   │
                                   ├─► rejected            （业务终态：风控拒、KYC 失败）
                                   │
                                   └─► compensating ──┬─► compensated
                                                       │
                                                       └─► compensation_failed
                                                            （需运营者 gRPC 介入；
                                                             详见「运营者冲正」）
```

| 状态 | 含义 |
|------|------|
| `preparing` | 准备中：读客户/账户快照、KYC/黑名单校验、构造不可变 `TransferContext`。 |
| `ready` | 准备完成，等待引擎首次调度。 |
| `running` | 已分发当前 Action 命令，等待下游结果事件（每个 Action 15 秒操作超时；超时由恢复循环重发，不抛弃实例）。 |
| `succeeded` | 三个 Action 全部成功；账务已过账、冻结已 capture。终态。 |
| `rejected` | 业务终态失败（风控拒绝、活动状态无效、 insufficient funds）；已按需补偿。终态。 |
| `compensating` | 触发了逆序补偿（任一 forward Action 终态失败或显式 `reverse`）。 |
| `compensated` | 所有已成功 Action 均已逆序补偿。终态。 |
| `compensation_failed` | 补偿在 `CompensationMaxAttempts=5` 次内未成功；实例卡住等待运营者冲正。 |

引擎默认：`ExecuteMaxAttempts=3`、`CompensationMaxAttempts=5`、`OperationalDeadline=2m`、Action 操作超时 15 s。超过 `OperationalDeadline` 不删除/不抛弃实例，仅记录错误并安排 wake-up——**durable 工作流从不静默丢失**。

### Action / Compensation 序列

forward 三个 Action（顺序执行，下游 consumer 在 risk / core-banking）：

| # | Action | 下发命令（routing key） | 接受的结果事件 | 含义 |
|---|--------|------------------------|----------------|------|
| 0 | `AuthorizeRisk` | `risk.authorize-payment.v1` | `risk.payment-authorized.v1` / `risk.payment-rejected.v1` | 风控授权（KYC、黑名单、规则） |
| 1 | `PlaceFundsHold` | `core.place-hold.v1` | `core.hold-placed.v1` / `core.hold-failed.v1` | 在 core-banking 预冻结付款人账户金额 |
| 2 | `PostLedgerTransfer` | `core.post-held-transfer.v1` | `core.transfer-posted.v1` / `core.transfer-failed.v1` | 复式过账转账（冻结转 capture，借贷同时落账） |

补偿（任一 forward Action 终态失败时，按**逆序**对每个已 `succeeded` 的 Action 单独下发补偿命令；不会自动跳过任何金融步骤）：

| 原 Action | 补偿命令（routing key） | 接受的结果事件 | 含义 |
|-----------|------------------------|----------------|------|
| `PostLedgerTransfer` | `core.reverse-transfer.v1` | `core.transfer-reversed.v1` / `core.transfer-reverse-failed.v1` | 反向分录冲销已过账的转账 |
| `PlaceFundsHold` | `core.release-hold.v1` | `core.hold-released.v1` / `core.hold-release-failed.v1` | 释放此前预冻结的金额 |
| `AuthorizeRisk` | `risk.void-payment-authorization.v1` | `risk.payment-authorization-voided.v1` | 作废风控授权记录 |

Action 之间通过 `priorActionOutput(actions, name)` 按语义名读取上游 Output（例如 `PostLedgerTransfer` 从 `PlaceFundsHold` 的 Output 中取 `hold_id`），不依赖硬编码位置索引。

失败分类（`ErrorClass`，由 consumer 在失败 payload 上盖戳，引擎根据类别决定 retry/compensate/leave-running）：

- `business_rejected` → 终态，触发补偿（如风控拒、余额不足）
- `transient_failure` → 可重试，实例保持 `running`（broker/依赖暂时不可用）
- `invariant_violation` → 终态（账务不平、hold 状态错），触发补偿
- `invalid_message` → 终态结构错（未知消息类型；forward 方向保持 running 待恢复，不立即补偿）
- `unknown_outcome` → 不识别的 payload，保持 running 等待恢复循环重发

## 跨服务聚合端点

服务经内部 gRPC 协作，**不跨库查询**：

```bash
# customer 查本库账户关系，再调用 core-banking gRPC 获取账户资料
curl -sf localhost:18000/api/v1/customers/C0000001/accounts

# payment 查本库转账，再调用 core-banking 和 customer gRPC 获取双方资料
curl -sf localhost:18000/api/v1/payments/transfers/PT000000000001/parties
```

预期：`/accounts` 返回该客户的 core 账户资料；`/parties` 返回转账双方账号 + 户主客户姓名。

loan/wealth 只读端点示例（Spec B-4b）：

```bash
curl -sf localhost:18000/api/v1/loan/accounts
curl -sf localhost:18000/api/v1/loan/accounts/{loan_no}/profile
curl -sf localhost:18000/api/v1/wealth/holdings/{holding_id}/profile
```

## 服务调用拓扑与边界

| 边界 | 用途 | 协议/交换机 | 谁用 |
|------|------|-----------|------|
| 同步只读 | 跨域读取（客户、账户） | **内部 gRPC**（customer/core-banking 在 `:9090`） | reward/risk/loan/wealth/payment.preparation 都经 `platform/serviceclient` 拨号 |
| 异步命令（saga forward + compensation） | payment 下发到下游执行 | **`bank.commands`（topic）** | payment 发；risk / core-banking 消费 |
| 异步结果事件 | 下游回报 Action 成败 | **`bank.events`（topic）** | risk / core-banking 发；payment 消费（`payment.workflow.events` 队列） |
| 领域完成事件 | payment 完成后通知下游 | **`bank.events`**，`payment.completed` 路由键 | payment 发；reward 消费（`reward.payment-events` 队列，发积分） |
| 运营者 gRPC | 干预卡住的补偿（运维面） | **admin gRPC `:9091`**（`payment-admin` headless Service） | 仅 label `role=bank-operator` 的 Pod 可达 |

**服务发现**（DNS，round-robin 负载均衡）：

- Compose：服务名（如 `core-banking`、`customer`）在 `bank-data` 网络上直接解析为多 IP。
- Kubernetes base：7 个 REST ClusterIP（`<name>`, http=8080）+ 7 个 headless gRPC（`<name>-grpc`, clusterIP=None, publishNotReadyAddresses=false, grpc=9090）+ 1 个 headless admin（`payment-admin`, admin-grpc=9091）。headless + publishNotReadyAddresses=false 确保 gRPC 客户端只拨到已就绪的 Pod。
- gRPC 拨号字符串：`CUSTOMER_GRPC_TARGET=dns:///customer:9090`、`CORE_BANKING_GRPC_TARGET=dns:///core-banking:9090`、`ADMIN_GRPC_ADDR=:9091`。

## 扩缩容

```bash
# 仅扩缩指定服务，不动依赖
make scale SERVICE=payment REPLICAS=3
make scale SERVICE=core-banking REPLICAS=2
```

副本默认（见上表）：core-banking / payment / risk = 2；customer / reward / loan / wealth = 1。Kubernetes base 对 core-banking/payment/risk 设了 PDB（`minAvailable=1`），所有 7 服务都挂了 CPU 80% 驱动的 HPA，`minReplicas` 对齐 compose 默认。

## 运行拓扑

- **网关**：Traefik v3 是唯一发布到宿主机的端口（`18000:8080`）；只路由公开 `/api/v1/...` REST 前缀。
- **专用数据库**：七个 PostgreSQL 16 实例（`core-banking-db` … `wealth-db`），各自独立卷，不存在跨库 SQL / 外部表 / 共享库权限。
- **消息中间件**：RabbitMQ 4，四个交换机：`bank.commands` / `bank.events`（topic）+ `bank.retry` / `bank.dlx`（direct）。命令/事件队列各带 `.retry`（2 秒 TTL）+ `.dlq` 伴侣。完整绑定见 `deploy/rabbitmq/definitions.json`。
- **内部 gRPC**：`customer:9090` 与 `core-banking:9090` 对内暴露只读查询；其余五服务仅起 REST，作为 gRPC 消费方。
- **副本默认**：core-banking / payment / risk = 2；customer / reward / loan / wealth = 1。
- **本地资源预算**：全栈约 6–8 GB 内存。应用容器限 512 MB / 1 CPU，PostgreSQL 限 768 MB，RabbitMQ 限 768 MB，Traefik 限 256 MB。
- **弹性扩缩**：`make scale SERVICE=payment REPLICAS=3`（仅扩缩指定服务，不动依赖）。
- **安全基线**：应用容器 `read_only` + `tmpfs:/tmp` + `cap_drop: ALL` + `no-new-privileges`。

## 可观测性

每个服务在 8080 上暴露 `/livez` / `/readyz` / `/metrics`（Prometheus 格式）+ `/healthz`。`/readyz` 在优雅关闭 drain 期间返回 503，Traefik 与 Kubernetes Ingress 用它做 drain。

```bash
make observability        # 拉 otel-collector / prometheus / grafana / jaeger（独立 bank-obs 网络）
make trace-smoke          # 提交一笔支付，断言 Jaeger 收到 REST+gRPC+messaging+workflow 跨层 trace
make observability-check  # jq 校验 dashboard JSON + promtool 校验 Prometheus 配置 + alerts
make observability-down   # 仅下掉可观测性 overlay 容器
```

**注意**：`make trace-smoke` 需要先 `make observability` + `make smoke`（后者会 apply smoke overlay，开启 `BANK_TEST_FAILURES_ENABLED=true`，使 payment 能用确定性 idempotency-key 前缀提交）。trace-smoke 本身走 success 路径，不依赖故障注入。

**Jaeger**：host 端口**不**发布——通过 `bank-obs` 网络内 `docker run --rm --network bank-obs curlimages/curl:8.10.1` 查询，符合「内部观测面不外暴露」的安全基线。

**Grafana**：3 个 dashboard（`deploy/grafana/dashboards/`）——`payment-workflows`（状态计数、Action p95 时延、补偿失败、最长等待）、`message-reliability`（发件箱年龄、收件箱去重、consumer lag）、`core-ledger`（过账/冲正计数、不变量失败）。

**Prometheus 告警**（`deploy/prometheus/alerts.yaml`，5 条；`for` 0–1m）：

| 告警 | 表达式 | 触发含义 |
|------|--------|---------|
| `WorkflowWaitingStuck` | `max(workflow_waiting_age_seconds) > 60` | 1 分钟内有工作流等待结果超过 60 秒 |
| `WorkflowCompensationFailure` | `sum(workflow_compensation_failures_total) > 0` | 1 分钟内出现补偿失败 |
| `OutboxBacklog` | `max(outbox_oldest_age_seconds) > 30` | 发件箱积压超 30 秒未投递 |
| `ConsumerLagHigh` | `sum(rabbitmq_consumer_lag) > 100` | 消费者滞后超 100 条 |
| `LedgerInvariantFailure` | `sum(ledger_invariant_failures_total) > 0` | 账务不变量失败（**`for: 0m`**——立即触发） |

## 死信队列与故障恢复

**RabbitMQ 拓扑**（`deploy/rabbitmq/definitions.json`）：

- **topic**：`bank.commands`（命令）、`bank.events`（事件）——通配符绑定（`risk.#`、`core.#` 等）确保 saga 所有 routing key 都能命中 consumer。
- **direct**：`bank.retry`（重试）、`bank.dlx`（死信终点）。
- 每个消费队列都有 `.retry` 伴侣（`x-message-ttl: 2000` + `x-dead-letter-exchange` 指回源 topic）+ `.dlq`（终端死信）。

**投递语义**：**至少一次**，安全由两件事保证：

- **事务发件箱**（publisher 侧）：状态变更和发件箱行在同一 PostgreSQL 事务里写。每服务一个 drain 循环 poll 发件箱，`claim_token`/`claimed_at` 让多实例 drain 不双发。RabbitMQ Publisher Confirm 确认投递成功后再标 dispatched。
- **幂等收件箱**（consumer 侧）：每个 consumer 在 apply 前先写 `inbox_event(event_id, ...)`，重复投递被去重。这是 at-least-once 安全的核心。

**重试与死信路径**：consumer 处理失败时按 `RetryPolicy`（默认 `MaxAttempts=5`）路由到 `bank.retry` 对应队列；2 秒 TTL 到期后 dead-letter 回源 topic 重新投递。重试次数耗尽后，消息路由到对应 `.dlq` 队列供人工检查（**不再自动重投**）。

**workflow 实例层面的对应关系**：
- consumer 侧的 `transient_failure`（broker/依赖暂不可用）→ 消息进 retry → 重新投递。工作流 Action 的 15 秒操作超时由 payment 引擎的恢复循环重发命令。
- 终态失败（`business_rejected` / `invariant_violation` / `invalid_message`）→ 工作流进入 `compensating`。
- 补偿 `transient_failure` 次数耗尽（`CompensationMaxAttempts=5`）→ 工作流 `compensation_failed`，等待运营者冲正（见下）。

## 运营者冲正（admin gRPC，:9091）

支付工作流提供**两个** protected RPC（`proto/bank/payment/v1/workflow_admin.proto`），由 payment 服务在独立 admin gRPC 端口 `:9091` 上发布（`payment-admin` headless Service），**不出现在网关**：

| RPC | 用途 |
|-----|------|
| `RetryCompensation(workflow_id, reason)` | 对卡在 `compensation_failed` 的实例重发补偿命令。完全自动化路径。 |
| `RecordReconciliation(workflow_id, action_name, external_reference, reason)` | 对卡住的补偿 Action 用一份**不可变外部对账引用**强制 resolve。调用前 server 会先 `Reconciler.ValidateReconciliation` 校验 core-banking 当前真实状态（funds-hold Action：hold 已释放；ledger-transfer Action：反向凭证已存在 + 借贷平衡），校验通过才落审计 + resolve。 |

**访问控制（三层）**：

1. **NetworkPolicy**（`deploy/k8s/base/networking.yaml`）：`payment-admin-ingress` 默认拒绝 9091 入站；只有带 `role=bank-operator` label 的 Pod 可达。
2. **token 校验**：调用方必须在 gRPC metadata 里带 `x-bank-operator-token`；server 用 `crypto/subtle.ConstantTimeCompare` 常数时间比较。token 从环境变量 `BANK_OPERATOR_TOKEN` 读取；**空值即 fail-closed**（startup 警告，所有 RPC 拒绝）。
3. **不可变审计**：`workflow_operator_audit` 表带 `BEFORE UPDATE OR DELETE` 触发器；每次 `RetryCompensation` / `RecordReconciliation` 与状态变更在**同一事务**内 INSERT 一条审计记录（operator / action / reference / reason / prev_state / new_state / created_at），永不修改。

`RecordReconciliation` 需要 core-banking 暴露 hold 状态与反向凭证查询 RPC 才能完整工作；模板带一个 fail-closed placeholder，未注入真实 inspector 时返回 `FailedPrecondition`。`RetryCompensation` 立即可用。

**注入 token**：

- Compose：默认不设；如需本地演练 admin RPC，在 `.env` 或 compose override 里设 `BANK_OPERATOR_TOKEN`。
- Dev overlay：`deploy/k8s/overlays/dev/secret.yaml` 内置可读字面量 `dev-operator-token`（标 `dev-only`）。
- Prod overlay：`deploy/k8s/overlays/prod/secret-contract.yaml` 用 SecretProviderClass 契约（无明文）从外部 vault 投射。

## 失败注入 smoke 套件（10 gate）

`make smoke`（`test/smoke.sh`）运行 10 个 gate，覆盖 saga 的成功/失败/恢复路径。失败注入由 `internal/platform/testfail` 提供，**仅**当 `BANK_TEST_FAILURES_ENABLED=true` 且 workflow id 以特定前缀（`smoke-reject-` / `smoke-insuff-` / `smoke-transient-` / `smoke-compfail-`）开头时触发；`compose.smoke.yaml` 是唯一开启该 env 的地方，且只由 smoke 脚本 apply。

| # | Gate | 断言 |
|---|------|------|
| 1 | replicas | core-banking / payment / risk 各 ≥2 健康容器 |
| 2 | success | 提交一笔 happy-path，poll 至 `succeeded` |
| 3 | risk-reject | `smoke-reject-` → `compensated`/`rejected`；无 hold、无 voucher |
| 4 | insufficient | `smoke-insuff-` → `compensated`；授权作废 |
| 5 | transient | `smoke-transient-` → `compensated`；hold 释放 |
| 6 | duplicate | 同 Idempotency-Key 两次 → 一个 workflow_id、一个 voucher |
| 7 | takeover | 中途 kill 一个 payment 容器 → 工作流仍成功 |
| 8 | reverse | 冲正一个 succeeded 支付 → `reversed=true`；voucher_reversal 行存在 |
| 9 | compensation-failed | `smoke-compfail-` → `compensation_failed` |
| 10 | negative probes | `/internal/*` 经网关 404；host 9090/9091 关闭；admin gRPC 仅 in-network 受 token 保护 |

## Kubernetes 拓扑（base + dev + prod overlay）

base（`deploy/k8s/base`）+ 两个 overlay（`deploy/k8s/overlays/{dev,prod}`）。

### base —— 纯应用层

`kubectl kustomize deploy/k8s/base` 渲染**纯应用层**清单：7 Deployment + 15 Service（7 REST ClusterIP + 7 headless gRPC + 1 headless admin）+ 1 Ingress（仅公开 REST）+ 3 PDB（core-banking/payment/risk）+ 7 HPA（CPU 驱动）+ 1 ConfigMap + 10 NetworkPolicy（默认拒绝 + allow matrix）。

**base 不包含任何有状态资源**——没有 StatefulSet、PV/PVC、Secret、PostgreSQL、RabbitMQ；可运行的运行态依赖由 dev/prod overlay 注入。

### dev overlay —— 自包含可运行

`kubectl apply -k deploy/k8s/overlays/dev` 拉起整套可运行拓扑：base + 8 StatefulSet（7 PostgreSQL + 1 RabbitMQ，各自 PVC + headless Service + readiness/liveness 探针）+ 1 dev Secret（`stringData` 可读字面量）+ 2 data-plane NetworkPolicy。所有 stateful 资源和 Secret 标 `bank.jiade/unsafe: dev-only`。

### prod overlay —— 外部状态 + SecretProviderClass

`kubectl kustomize deploy/k8s/overlays/prod` 渲染为生产形状：base + 8 ExternalName Service（指向外部托管 PostgreSQL/RabbitMQ DNS）+ 1 ConfigMap（可配置的 DNS 字面量）+ 1 SecretProviderClass（secrets-store-csi 驱动契约，无明文，operator 填 `parameters`）。**0 StatefulSet、0 Secret、0 PVC**；Kustomize `replacements` 把 ConfigMap 字面量映射到 ExternalName Service 的 `spec.externalName`。

三个 overlay 都保持 base 的安全基线：公开 Ingress 只路由 `/api/v1/...` REST，**绝不**路由 `/internal/*`、gRPC 9090、admin 9091；`payment-admin-ingress` NetworkPolicy 把 admin gRPC 锁在 `role=bank-operator` label 之后。

## 状态化高可用非目标

本模板**不声称状态化高可用**。具体：

- 本地 Compose：PostgreSQL 与 RabbitMQ 各为**单副本**，仅用于开发与集成测试。
- dev overlay：PostgreSQL/RabbitMQ 各自单副本 StatefulSet（`replicas: 1`）——可运行，不 HA。
- prod overlay：把 PG/RabbitMQ DNS 完全委托给外部托管服务（ExternalName）；**不**包含 StatefulSet、不声称 PostgreSQL/RabbitMQ HA，亦不内置 Patroni/quorum queue/federation。把 ExternalName 指向你的 operator-managed StatefulSet 或云托管服务后再上正式流量。

无状态服务的 HA 由 Deployment + PDB（core-banking/payment/risk，`minAvailable=1`）+ HPA 提供。

## 清理

```bash
make down                # compose down：停 + 删容器 + 删卷（--volumes）
make observability-down  # 仅下掉可观测性 overlay 容器
```

**警告**：`make down` 会执行 `--volumes --remove-orphans`，**抹掉全部 7 个 PostgreSQL 数据卷和 RabbitMQ mnesia 卷**。RabbitMQ 的 `definitions.json`（交换机/队列/绑定）仅在**首次卷初始化**时载入——`make down` 后再 `make up` 是干净拓扑，但**手动改过 `definitions.json` 后必须 `make down` 才能生效**（仅重启容器不会重读）。

Kubernetes 集群清理（独立集群，如果你 apply 过）：

```bash
kubectl delete -k deploy/k8s/overlays/dev    # 或 overlays/prod
```

dev overlay 的 PVC 不会随 `kubectl delete -k` 自动删除；手动 `kubectl delete pvc -l bank.jiade/unsafe=dev-only -n bank` 才能彻底清掉卷。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚——既护 seed 也护 B-3 运行时记账/冲正，亦护 saga `PostLedgerTransfer` 的 forward/补偿路径。customer/payment 无总账。
- 工作流引擎从不静默删除或抛弃实例；超过 `OperationalDeadline` 仅记录并安排 wake-up。
- 运营者操作永远留下不可变审计行（同一事务），不可 UPDATE/DELETE。

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。7 进程 + 7 独立 PostgreSQL + RabbitMQ + Traefik 网关；同步跨域读取走内部 gRPC，异步命令/事件走 RabbitMQ；每服务分层 `api → service → repo → domain`，domain 零外部依赖。支付 durable saga 由 payment 内的 `internal/platform/workflow` 引擎编排。
