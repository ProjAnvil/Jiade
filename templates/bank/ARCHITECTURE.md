# bank 架构

## 服务与数据所有权

| 服务 | REST / gRPC | 独占数据库 | 默认副本 | 职责 |
|------|-------------|------------|----------|------|
| core-banking | 8080 / 9090 | core_db | 2 | 活期/定存、复式记账、余额、过账/冲正 |
| customer | 8080 / 9090 | cust_db | 1 | 客户信息、账户关系 |
| payment | 8080 / — | pay_db | 2 | 转账、商户、消费 |
| reward | 8080 / — | reward_db | 1 | 积分、优惠券、活动 |
| risk | 8080 / — | risk_db | 2 | 风控规则、事件、黑名单 |
| loan | 8080 / — | loan_db | 1 | 借据、还款、逾期、余额快照 |
| wealth | 8080 / — | wealth_db | 1 | 产品、净值、持仓、订单、收益 |

每个服务是独立 Go 进程，只连接自己**独占**的 PostgreSQL 16 实例与卷
（`core-banking-db` … `wealth-db`）。不存在外部表、跨库 SQL 或共享数据库访问
权限假设。容器端口 8080（REST）/ 9090（gRPC）不发布到宿主机——只有 Traefik 网关
发布 `18000:8080`。

## 分层

每个业务域采用 `api → service → repo → domain` 纵切：

- `api`：HTTP handler 与路由（`httpx.Server`，提供 `/healthz` `/livez` `/readyz`
  `/metrics` 与优雅关闭）；customer / core-banking 额外注册 gRPC 查询服务。
- `service`：用例编排和业务规则。
- `repo`：本服务数据库访问；聚合端点所需的跨域只读数据通过
  `platform/serviceclient` 调用其他服务的内部 gRPC。
- `domain`：纯领域模型，不依赖数据库或 HTTP/gRPC 框架。

`platform/pg` 管理数据库连接，`platform/migrate` 执行迁移，
`platform/grpcx` 提供 DNS 负载均衡的 gRPC 客户端与健康感知的 gRPC 服务端，
`platform/serviceclient` 提供 gRPC 适配器（`CustomerReader` / `AccountReader`），
`platform/messaging` 提供 RabbitMQ 发件箱/收件箱原语。

## 服务调用拓扑

```text
                   ┌─ gRPC (sync read) ─┐    ┌─ RabbitMQ (async) ─┐
customer ────────> core-banking             payment ──> bank.events
payment  ────────> core-banking             risk    ──> bank.commands
payment  ────────> customer                 core    ──> bank.commands
reward   ────────> customer                 reward  ──> bank.events (consumer)
risk     ────────> customer
loan     ────────> customer
wealth   ────────> customer
```

- **同步只读**：customer 与 core-banking 在 `:9090` 暴露 gRPC 查询服务
  （`CustomerQueryService` / `AccountQueryService`）。容器内通过
  `CUSTOMER_GRPC_TARGET=dns:///customer:9090` 与
  `CORE_BANKING_GRPC_TARGET=dns:///core-banking:9090` 服务发现，客户端走
  round_robin 负载均衡。
- **异步命令/事件**：RabbitMQ 交换机 `bank.commands`（topic）与
  `bank.events`（topic）。每个命令/事件队列各带 `.retry`（2 秒 TTL，到期重回源
  交换机）与 `.dlq`（终端死信）。发布走事务发件箱 + Publisher Confirm，消费走
  Inbox 幂等 + 手动 ack。

## 跨服务聚合端点

| 端点 | 编排 |
|------|------|
| `GET /api/v1/customers/{cust_id}/accounts` | customer 查本库关系，逐个调用 core-banking gRPC 查账户 |
| `GET /api/v1/payments/transfers/{txn_id}/parties` | payment 查本库转账，调用 core-banking gRPC 查账户归属，再调用 customer gRPC 查姓名 |
| `GET /api/v1/reward/customers/{cust_id}/profile` | reward 查本库积分，调用 customer gRPC 查客户 |
| `GET /api/v1/risk/events/{event_id}` | risk 查本库事件，调用 customer gRPC 查客户 |
| `GET /api/v1/loan/accounts/{loan_no}/profile` | loan 查本库借据，调用 customer gRPC 查客户 |
| `GET /api/v1/wealth/holdings/{holding_id}/profile` | wealth 查本库持仓，调用 customer gRPC 查客户 |

上游 gRPC 不可用、超时或返回非 OK 时，聚合端点返回错误，不会回退到跨库读取。

## Seed 数据流

`cmd/seed` 只负责数据库与 fixture：

1. 连接 7 个**专用** PostgreSQL 主机（`core-banking-db` … `wealth-db`）。
2. 执行 7 套迁移（每库先跑 `shared.sql` 建发件箱/收件箱表，再跑域迁移）。
3. 按 core → customer → payment → reward → risk → loan → wealth 顺序灌数。

Seed 不安装 PostgreSQL 扩展，也不创建外部表。确定性 fixture、三因子事件流、
逐日余额/NAV 滚存逻辑保持不变。

## Kubernetes 应用基线

`deploy/k8s/base` 是**纯应用层** kustomize 基线，`kubectl kustomize` 即可渲染：

- 7 Deployment（共享单一容器镜像，envFrom `bank-config` ConfigMap 取非密默认值；
  DB_HOST / DB_NAME / gRPC target 逐 Deployment 设置）。
- 14 Service：7 个 REST ClusterIP（`<name>`，端口 http=8080）+ 7 个 headless
  gRPC（`<name>-grpc`，clusterIP=None、publishNotReadyAddresses=false，端口
  grpc=9090），确保 gRPC 客户端只拨到已就绪的 Pod。
- 1 Ingress（ingressClassName=traefik），仅路由公开 `/api/v1/...` REST 前缀，
  绝不路由 `/internal/*` 或 9090。
- 3 PDB（core-banking / payment / risk，minAvailable=1）+ 7 HPA（CPU 80%，
  minReplicas 对齐副本默认值；依赖 Metrics Server）。

**此基线不含任何有状态资源**（无 StatefulSet / PV / PVC / Secret / PostgreSQL /
RabbitMQ），runnable 运行态由后续 operational-closure overlay 注入。本地 Compose
拓扑**不声称有状态高可用**：PostgreSQL 与 RabbitMQ 各为单副本，面向开发与集成测试。

## 金融不变量

- 金额使用 `int64` 分，禁用浮点。
- 复式记账只在 core-banking：过账强制借贷平衡，失败时整笔事务回滚。
- 跨服务查询是只读编排，不跨服务共享数据库事务。
