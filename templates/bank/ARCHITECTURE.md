# bank 架构

本文描述 bank 模板的运行时拓扑、支付 durable saga 的数据流、消息可靠性保证，以及部署形态决策背后的理由。操作命令（`make up` / `make smoke` / `make observability` / `make scale` 等）见 [README.md](README.md)。

## 服务与数据所有权

| 服务 | REST / gRPC | 独占数据库 | 默认副本 | 职责 |
|------|-------------|------------|----------|------|
| core-banking | 8080 / 9090 | core_db | 2 | 活期/定存、复式记账、余额、过账/冲正、**资金冻结（hold/place/release/capture）** |
| customer | 8080 / 9090 | cust_db | 1 | 客户信息、账户关系 |
| payment | 8080 / 9091 (admin) | pay_db | 2 | 转账、商户、消费、**durable 支付工作流引擎** |
| reward | 8080 / — | reward_db | 1 | 积分、优惠券、活动 |
| risk | 8080 / — | risk_db | 2 | 风控规则、事件、黑名单、**支付授权（authorize/void）** |
| loan | 8080 / — | loan_db | 1 | 借据、还款、逾期、余额快照 |
| wealth | 8080 / — | wealth_db | 1 | 产品、净值、持仓、订单、收益 |

每个服务是独立 Go 进程，只连接自己**独占**的 PostgreSQL 16 实例与卷
（`core-banking-db` … `wealth-db`）。不存在外部表、跨库 SQL 或共享数据库访问
权限假设。容器端口 8080（REST）/ 9090（gRPC）/ 9091（admin gRPC，仅 payment）
不发布到宿主机——只有 Traefik 网关发布 `18000:8080`。

## 分层

每个业务域采用 `api → service → repo → domain` 纵切：

- `api`：HTTP handler 与路由（`httpx.Server`，提供 `/healthz` `/livez` `/readyz`
  `/metrics` 与优雅关闭）；customer / core-banking 额外注册 gRPC 查询服务；
  payment 额外注册 admin gRPC（独立 `:9091` 监听器）。
- `service`：用例编排和业务规则。
- `repo`：本服务数据库访问；聚合端点所需的跨域只读数据通过
  `platform/serviceclient` 调用其他服务的内部 gRPC。
- `domain`：纯领域模型，不依赖数据库或 HTTP/gRPC 框架。

`platform/pg` 管理数据库连接，`platform/migrate` 执行迁移（dollar-quote 感知的
SQL 分句器兼容 `$$ … $$` PL/pgSQL 函数体），`platform/grpcx` 提供 DNS 负载均衡
的 gRPC 客户端与健康感知的 gRPC 服务端，`platform/serviceclient` 提供 gRPC 适配器
（`CustomerReader` / `AccountReader`），`platform/messaging` 提供 RabbitMQ
发件箱/收件箱原语，`platform/workflow` 提供 durable saga 引擎，`platform/telemetry`
提供 OTLP trace 导出器。

## 服务调用拓扑

```text
                   ┌─ gRPC (sync read) ─┐    ┌─ RabbitMQ (async) ─────────────────┐
customer ────────> core-banking             payment ──> bank.commands (saga forward + compensation)
payment  ────────> core-banking             risk    ──> bank.events     (risk.* 结果事件)
payment  ────────> customer                 core    ──> bank.events     (core.* 结果事件)
reward   ────────> customer                 payment ──> bank.events     (payment.completed 完成事件)
risk     ────────> customer                 reward  ──> bank.events consumer (reward.payment-events)
loan     ────────> customer
wealth   ────────> customer

operator (label role=bank-operator) ──admin gRPC :9091──> payment (WorkflowAdminService)
```

- **同步只读**：customer 与 core-banking 在 `:9090` 暴露 gRPC 查询服务
  （`CustomerQueryService` / `AccountQueryService`）。容器内通过
  `CUSTOMER_GRPC_TARGET=dns:///customer:9090` 与
  `CORE_BANKING_GRPC_TARGET=dns:///core-banking:9090` 服务发现，客户端走
  round_robin 负载均衡。
- **异步命令/事件**：RabbitMQ 交换机 `bank.commands`（topic，saga 命令）与
  `bank.events`（topic，结果事件 + 领域完成事件）。命令/事件队列各带 `.retry`
  （2 秒 TTL，到期重回源交换机）与 `.dlq`（终端死信）。发布走事务发件箱 +
  Publisher Confirm，消费走 Inbox 幂等 + 手动 ack。
- **运营者 gRPC**：payment 在 `:9091` 单独监听 admin 服务，**不**经 Traefik，
  由 `payment-admin` headless Service 暴露。NetworkPolicy 默认拒绝 9091 入站；
  只有 `role=bank-operator` label 的 Pod 可达。所有 RPC 需 `x-bank-operator-token`
  metadata（常数时间比较）。

## 跨服务聚合端点

| 端点 | 编排 |
|------|------|
| `GET /api/v1/customers/{cust_id}/accounts` | customer 查本库关系，逐个调用 core-banking gRPC 查账户 |
| `GET /api/v1/payments/transfers/{txn_id}/parties` | payment 查本库转账，调用 core-banking gRPC 查账户归属，再调用 customer gRPC 查姓名 |
| `GET /api/v1/reward/customers/{cust_id}/profile` | reward 查本库积分，调用 customer gRPC 查客户 |
| `GET /api/v1/risk/events/{event_id}` | risk 查本库事件，调用 customer gRPC 查客户 |
| `GET /api/v1/loan/accounts/{loan_no}/profile` | loan 查本库借据，调用 customer gRPC 查客户 |
| `GET /api/v1/wealth/holdings/{holding_id}/profile` | wealth 查本库持仓，调用 customer gRPC 查客户 |

上游 gRPC 不可用、超时或返回非 OK 时，聚合端点返回错误，**不会回退到跨库读取**。

## 支付工作流 saga（durable）

payment 服务内置一个 domain-neutral 的 durable 工作流引擎
（`internal/platform/workflow`），版本 1 名为 `payment-transfer`。每次请求落库为一个
不可变 `Instance`（status / current_action / revision / lease / actions 全在 PostgreSQL）。

### Preparation（不可变上下文）

`workflows/prepare.go` 在工作流开始前一次性读取付款人客户/账户快照、KYC 校验、
活动状态校验，构造一个不可变 `TransferContext`（金额、双方账号、客户 ID、币种、
trace 上下文）。后续所有 Action 都从 `View.Instance.PreparedContext` 读这个上下文，
从不修改。

### Action 序列（forward）

```
AuthorizeRisk → PlaceFundsHold → PostLedgerTransfer
   risk            core-banking       core-banking
```

| Action | 命令（routing key） | 接受结果 | 失败分类 |
|--------|--------------------|---------|---------|
| `AuthorizeRisk` | `risk.authorize-payment.v1` | `risk.payment-authorized.v1` / `risk.payment-rejected.v1` | rejected → 业务终态（触发补偿） |
| `PlaceFundsHold` | `core.place-hold.v1` | `core.hold-placed.v1` / `core.hold-failed.v1` | business_rejected → 终态补偿；transient → 重发 |
| `PostLedgerTransfer` | `core.post-held-transfer.v1` | `core.transfer-posted.v1` / `core.transfer-failed.v1` | invariant_violation → 终态补偿 |

每个 Action 的操作超时 15 秒（`actionDispatchDeadline`）：超过则 payment 引擎恢复循环
重新下发命令（不放弃实例）。下游 consumer 在结果事件 payload 上盖戳 `error_class`
（business_rejected / transient_failure / invariant_violation / invalid_message），
引擎直接消费，无需 re-derive。

### 补偿序列（逆序）

任一 forward Action 终态失败时，引擎按**逆序**对每个已 `succeeded` 的 Action 下发
补偿命令。**不会自动跳过任何金融步骤**——每个已 succeeded 的 Action 都有自己的
Compensate dispatch。

| 原 Action | 补偿命令 | 接受结果 |
|-----------|---------|---------|
| `PostLedgerTransfer` | `core.reverse-transfer.v1` | `core.transfer-reversed.v1` / `core.transfer-reverse-failed.v1` |
| `PlaceFundsHold` | `core.release-hold.v1` | `core.hold-released.v1` / `core.hold-release-failed.v1` |
| `AuthorizeRisk` | `risk.void-payment-authorization.v1` | `risk.payment-authorization-voided.v1` |

补偿失败的 transient 次数耗尽（`CompensationMaxAttempts=5`）→ 实例进入
`compensation_failed`，等待运营者经 admin gRPC 介入（见下）。

### Action 间数据传递

Action 不通过硬编码位置索引找上游 Output，而是按语义名（`AuthorizeRisk` /
`PlaceFundsHold` / `PostLedgerTransfer`）经 `priorActionOutput(actions, name)` 读取。
例如 `PostLedgerTransfer` 从 `PlaceFundsHold` 的 Output JSON 中取 `hold_id`；
`PlaceFundsHold` 的补偿从 forward Output 中取 `hold_id` 用于释放。

### Saga 消息路由上下文

引擎在构造每个命令 envelope 时把 saga 路由字段盖戳上去：`correlation_id`
（来自 `StartRequest`，回退到 workflow_id）、`workflow_id`、`action_name`、
`command_id`、`message_id`（causation）。consumer 把这些字段原样回写到结果
envelope（`makeResultEnvelope`），使 payment 引擎的 `ApplyResult` 能用
`env.CommandID == action.CommandID` 校验回执。core-banking 的
`HeldTransferService.PostHeldTransfer` / `ReverseTransfer`（service 层、事务内构造
envelope）通过 `SagaRouting` 输入字段把同样的路由上下文盖到结果上。

## 消息可靠性模型

**至少一次**投递，安全由两层保证：

### 事务发件箱（publisher 侧）

每条状态变更事务在同一 PostgreSQL 事务里 INSERT 一条 `outbox_event` 行。每服务一个
drain 循环 poll 发件箱：

- `claim_token` / `claimed_at` 让多实例 drain 不双发——claimant 拿到 token 才发布。
- 通过 `ExchangeForRoutingKey` 按行决定目标交换机：版本化 `*.vN` 命令 → `bank.commands`，
  非版本化结果事件 → `bank.events`。这至关重要，因为 payment 发件箱**同时**持有 saga
  dispatch 命令（`risk.authorize-payment.v1` 等）和 `payment.completed` 完成事件——
  一个固定交换机会让一半流量 NO_ROUTE。
- RabbitMQ Publisher Confirm 确认投递后再标 dispatched。

### 幂等收件箱（consumer 侧）

每个 consumer 在 apply 前先 INSERT `inbox_event(event_id, ...)`；重复投递
（drain 重启、网络重试、RabbitMQ redelivery）的第二份会撞主键并被跳过。这是
at-least-once 安全的核心。

### 重试与死信拓扑

`deploy/rabbitmq/definitions.json` 定义：

- `bank.commands` / `bank.events`（**topic**）——主路由，通配符绑定（`risk.#` / `core.#` 等）。
- `bank.retry`（**direct**）——显式重试入口；每个消费队列的 `.retry` 伴侣设
  `x-message-ttl: 2000` + `x-dead-letter-exchange: <源 topic>` + `x-dead-letter-routing-key: <源队列名>`，
  2 秒到期后 dead-letter 回源 topic 重新投递。
- `bank.dlx`（**direct**）——终端死信；每个消费队列的 `.dlq` 伴侣绑定到 dlx 的
  `<queue>.dead` 路由键。重试次数耗尽的消息进入 `.dlq`，**不再自动重投**，供人工检查。

通配符 + 精确绑定共存：retry 队列的 `x-dead-letter-routing-key` 用的是**队列名**
（如 `core-banking.commands`、`payment.workflow.events`），它们与 `core.#` /
`risk.#` 通配符不匹配（连字符不是 dot 分隔符），所以额外的精确绑定保证 retry
dead-letter-back 路径仍能命中。

## 运营者冲正（admin gRPC）

payment 在独立 `:9091` 监听器上发布 `WorkflowAdminService`（proto 在
`proto/bank/payment/v1/workflow_admin.proto`）。**不出现在网关**；由 headless
`payment-admin` Service 在集群内暴露。

| RPC | 行为 |
|-----|------|
| `RetryCompensation(workflow_id, reason)` | 对 `compensation_failed` 实例重发补偿命令。 |
| `RecordReconciliation(workflow_id, action_name, external_reference, reason)` | 调用 `Reconciler.ValidateReconciliation` 校验 core-banking 当前真实状态（funds-hold Action 要求 hold 已释放；ledger-transfer Action 要求反向凭证已存在 + 借贷平衡），校验通过后在同一事务内 INSERT 不可变审计行 + resolve 补偿 Action。 |

三层保护：(1) NetworkPolicy 默认拒绝 9091 入站，仅 `role=bank-operator` Pod 可达；
(2) `x-bank-operator-token` 常数时间比较，空 token fail-closed；(3) `workflow_operator_audit`
表带 `BEFORE UPDATE OR DELETE` 触发器，永不修改。

`RecordReconciliation` 当前带 fail-closed placeholder `notConfiguredCoreBankingInspector`
（core-banking 还未暴露 hold 状态/反向凭证查询 RPC），未注入真实 inspector 时返回
`FailedPrecondition`。`RetryCompensation` 立即可用。

## Seed 数据流

`cmd/seed` 只负责数据库与 fixture：

1. 连接 7 个**专用** PostgreSQL 主机（`core-banking-db` … `wealth-db`）。
2. 执行 7 套迁移（每库先跑 `shared.sql` 建发件箱/收件箱表，再跑域迁移）。
3. 按 core → customer → payment → reward → risk → loan → wealth 顺序灌数。

Seed 不安装 PostgreSQL 扩展，也不创建外部表。确定性 fixture、三因子事件流、
逐日余额/NAV 滚存逻辑保持不变。

## Kubernetes 部署形态

`deploy/k8s/base` 是**纯应用层** kustomize 基线，`kubectl kustomize` 即可渲染：

- 7 Deployment（共享单一容器镜像，envFrom `bank-config` ConfigMap 取非密默认值；
  DB_HOST / DB_NAME / gRPC target 逐 Deployment 设置；payment 额外设 admin-grpc=9091 容器端口）。
- 15 Service：7 REST ClusterIP（`<name>`，http=8080）+ 7 headless gRPC（`<name>-grpc`，
  clusterIP=None、publishNotReadyAddresses=false，grpc=9090）+ 1 headless admin
  （`payment-admin`，admin-grpc=9091）。headless + publishNotReadyAddresses=false 确保
  gRPC 客户端只拨到已就绪的 Pod。
- 1 Ingress（ingressClassName=traefik），仅路由公开 `/api/v1/...` REST 前缀，
  绝不路由 `/internal/*` 或 9090/9091。
- 3 PDB（core-banking / payment / risk，minAvailable=1）+ 7 HPA（CPU 80%，
  minReplicas 对齐副本默认值；依赖 Metrics Server）。
- 10 NetworkPolicy（默认拒绝 Ingress+Egress + allow matrix：DNS egress、gateway→REST、
  internal gRPC、app gRPC egress、app 数据 egress :5432/:5672、Prometheus scrape、
  OTLP egress、payment-admin 9091 仅限 role=bank-operator）。

### dev overlay (`deploy/k8s/overlays/dev`)

可运行的自包含拓扑：base + 8 StatefulSet（7 PG + 1 RMQ，各自 PVC + headless Service +
exec readiness/liveness 探针）+ 1 dev Secret（`stringData` 可读字面量 `DB_PASSWORD=bank`、
`BROKER_URL=...`、`BANK_OPERATOR_TOKEN=dev-operator-token`）+ 2 data-plane NetworkPolicy
（开 :5432/:5672 给 7 个 app pod）。所有 stateful 资源和 Secret 标 `bank.jiade/unsafe: dev-only`。

### prod overlay (`deploy/k8s/overlays/prod`)

外部状态契约：base + 8 ExternalName Service（指向外部托管 PG/RabbitMQ DNS）+
1 ConfigMap（可配置 DNS 字面量，默认 `*.example.internal`）+ 1 SecretProviderClass
（secrets-store-csi 驱动契约，把 `DB_PASSWORD` / `BROKER_URL` / `BANK_OPERATOR_TOKEN`
从外部 vault 投射成 `bank-prod-secrets` Secret，无明文）。**0 StatefulSet、0 Secret、0 PVC**。
Kustomize `replacements` 把 ConfigMap 字面量映射到 ExternalName Service 的
`spec.externalName`，机制经过单独验证。

### 状态化 HA 非目标

base 与两个 overlay **都不声称 PostgreSQL/RabbitMQ HA**。Compose 是单副本；
dev overlay 用单副本 StatefulSet；prod overlay 完全委托给外部托管服务。无状态服务 HA
由 Deployment + PDB（core-banking/payment/risk，`minAvailable=1`）+ HPA 提供。

## 可观测性

每服务 8080 上暴露 `/livez`（liveness）、`/readyz`（readiness，drain 期间 503）、
`/metrics`（Prometheus 格式：请求时延、发件箱积压、收件箱去重、pgx 池统计、
工作流状态计数、Action 时延、补偿失败、账务不变量）。OpenTelemetry instrumentation
（otelgrpc / otelhttp）在所有 gRPC/HTTP 调用上传播 W3C trace 上下文；
`platform/telemetry.New` 在每个 service main 早期调用，把 OTLP 导出器注册到全局
provider。

可观测性 overlay（`compose.observability.yaml`）拉 4 个容器：OTel collector
（OTLP 接 :4317/:4318，trace 导 jaeger:4317）、Prometheus（DNS-SD scrape 7 个服务
:8080/metrics + collector :8888 + rabbitmq :15692，载入 alerts.yaml）、Grafana
（自动 provision Prometheus + Jaeger datasource + 3 个 dashboard）、Jaeger。
所有容器在独立 `bank-obs` 网络上，host 端口**不**发布。

## 金融不变量

- 金额使用 `int64` 分，禁用浮点。
- 复式记账只在 core-banking：过账强制借贷平衡，失败时整笔事务回滚。
- 跨服务查询是只读编排，不跨服务共享数据库事务。
- 工作流引擎从不静默删除或抛弃实例；超过 `OperationalDeadline=2m` 仅记录并安排 wake-up。
- 运营者操作永远在同一事务内留下不可变审计行（`workflow_operator_audit`，触发器禁止 UPDATE/DELETE）。

## 决策摘录

- **为什么用 durable 工作流引擎而不是分散的 saga？** 工作流的全部状态（forward Action
  状态、补偿状态、尝试次数、操作超时）都在 PostgreSQL 里，payment 容器崩溃重启后恢复
  循环按 `LeaseUntil` / `NextWakeupAt` 重新调度。takeover smoke gate（kill 一个 payment
  容器，工作流仍成功）证明这点。一个状态机字段（`Status`）暴露所有运维面，无需分散的
  per-saga 表。
- **为什么 gRPC 用于读取、RabbitMQ 用于命令/事件？** 聚合端点（loan/customer profile 等）
  需要当下事实，事件传播是 eventual —— 与 commerce 模板的 checkout 同样决策。Saga 命令是
  「请求某服务做事并等回执」的形态，天然异步；事件让 payment.completed 的多个订阅者
  （reward）解耦。
- **为什么 admin gRPC 单独监听而不是挂在主 gRPC 上？** 把 token-gated 运维面与公共
  只读查询面物理隔离：NetworkPolicy 可以在端口粒度上拒绝所有未授权入站，无需在应用
  interceptor 里区分 RPC；公共 Ingress 永远不可能误把 admin 路由出去。
- **为什么 `definitions.json` 通配符 + 精确绑定共存？** saga 命令是版本化的
  （`risk.authorize-payment.v1`），用 topic 通配符（`risk.#`）匹配；retry 队列的
  `x-dead-letter-routing-key` 用的是队列名（如 `core-banking.commands`），连字符不是
  dot 分隔符，必须用精确绑定兜底，否则 retry dead-letter-back 路径会 NO_ROUTE。
