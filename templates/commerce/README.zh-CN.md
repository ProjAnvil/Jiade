# Commerce 模板

[English README](README.md)

一个完整的电商后端缩影：商品/SKU、客户、库存预占、订单、支付/退款、分仓发货与物流追踪。六个
Go 微服务、六个服务独占的 PostgreSQL 库、RabbitMQ，以及一个 Traefik 网关——小到可以完整理解，
真到能端到端运行。

默认全栈面向**单台 Docker 主机（或 kind/minikube 节点）的 4–6 GB 内存预算**。更大的规模供
压测使用；见 [Seed 规模](#seed-规模)。

本模板属于 [jiade](../../README.zh-CN.md) 项目。用以下命令生成可运行副本：

```bash
jiade init --template commerce --dir ./myshop
cd myshop && make up
```

## 快速开始

前置条件：Docker（含 compose）、`make`、`curl`、`jq`。

```bash
# 1. 构建、迁移并按默认副本灌入拓扑。Makefile 目标会等待每个服务健康后返回。
make up

# 2. 探一下网关（主机端口 18100 是唯一发布到宿主机的端口）。
curl -fsS http://localhost:18100/api/v1/products?limit=1 | jq .
curl -fsS http://localhost:18100/api/v1/customers?limit=1 | jq .

# 3. 运行 Phase-B 验收脚本（见下方「失败注入」）。
make smoke

# 4. 全部拆除（含数据卷）。
make down
```

`make up` 一步完成 **构建 → 迁移 → 灌数 → 等待健康**。该操作幂等：重复运行会重建变更过的镜像、
重新应用迁移，并以 `--reset`（丢弃并重建确定性 fixture）重新灌数。

## 拓扑

六个 Go 服务及其后端存储。Traefik 是唯一对外可达的入口——没有任何服务或数据库端口发布到宿主机。

| 服务 | 副本 | 数据库 | 职责 |
|------|-----:|--------|------|
| catalog | 2 | catalog | 商品、SKU、价格快照 |
| customer | 1 | customer | 客户、地址 |
| inventory | 2 | inventory | SKU 库存、预占状态机 |
| order | 2 | order | 购物车、结账 saga、订单投影 |
| payment | 1 | payment | 支付意向/尝试、退款、webhook |
| fulfillment | 1 | fulfillment | 仓库分单、发货、物流追踪 |

每个服务独占一个 PostgreSQL 16 容器、数据库、schema 和命名卷（无共享库）。跨服务读取走 HTTP；
状态传播走 RabbitMQ topic 事件。数据流详见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 端点

外部路由（仅可通过 Traefik 网关 `:18100` 访问）：

| 方法 | 路径 | 服务 | 说明 |
|------|------|------|------|
| GET | `/api/v1/products` | catalog | 商品列表/搜索 |
| GET | `/api/v1/customers` | customer | 客户列表 |
| GET | `/api/v1/inventory` | inventory | SKU 库存水位 |
| POST/GET | `/api/v1/reservations/{order_id}` | inventory | 预占状态 |
| POST | `/api/v1/carts` / `/api/v1/carts/{id}/items` | order | 购物车生命周期 |
| POST | `/api/v1/checkouts` | order | 结账 saga 入口 |
| GET | `/api/v1/orders` / `/api/v1/orders/{id}` | order | 订单投影 |
| GET | `/api/v1/payments/orders/{id}` | payment | 支付意向视图 |
| POST | `/api/v1/payments/webhooks` | payment | 幂等 webhook 接入 |
| GET | `/api/v1/fulfillment/orders/{id}` | fulfillment | 发货 + 物流 |

内部路由（`/internal/v1/...`）仅可通过服务网络中的服务 DNS 访问。Traefik 与 Kubernetes Ingress
刻意不对这些路径配置规则。它们是服务间契约，不是网关兜底：客户端必须使用公开的 `/api/v1/...`
路由，而需要内部路由的工作负载必须在私有网络上按 DNS 调用所属服务。

每个响应都包含一个 `X-Service-Instance` 响应头，标明处理请求的副本。该响应头是负载均衡验证
探针——见下一节。

## 扩缩容

每个服务都有已批准的默认副本数（见上表）。可以按调用覆盖任意一个：

```bash
# 仅扩缩单个服务，不影响其他服务。
make scale SERVICE=order REPLICAS=4

# 或为单次 make up 覆盖默认值。
make up ORDER_REPLICAS=4 INVENTORY_REPLICAS=3
```

Makefile 将 `--scale <svc>=<n>` 标志传给 `docker compose up`。compose 文件中的
`deploy.replicas` 是默认值的唯一真相来源。

有状态后端服务（PostgreSQL、RabbitMQ）始终单副本——见[非目标](#非目标)。

### 负载均衡验证

Traefik 在各副本间 round-robin 负载均衡（无会话亲和）。由于每个副本设置了唯一的
`INSTANCE_ID`，且服务通过 `X-Service-Instance` 响应头返回该值，因此可以通过多次请求读取
该响应头来验证均衡器是否在分发流量：

```bash
# 当 catalog replicas=2 时，预期看到两个或更多不同的 instance ID。
for i in $(seq 1 12); do
  curl -fsS -D - http://localhost:18100/api/v1/products?limit=1 -o /dev/null \
    | awk -F': ' 'tolower($1)=="x-service-instance" {gsub("\r","",$2); print $2}'
  sleep 0.2
done | sort -u
```

这是 `test/smoke.sh` 的 gate 1。`make smoke` 端到端运行相同的检查（外加五个 gate）。

## 结账流程

Happy-path 结账是 order 服务编排的一个简短 saga：

1. **购物车** —— `POST /api/v1/carts` 创建购物车；
   `POST /api/v1/carts/{id}/items` 添加 SKU + 数量。
2. **结账** —— `POST /api/v1/checkouts`（带 `Idempotency-Key` 请求头）
   对照 customer/catalog/inventory 校验购物车并启动 saga。
   该端点立即返回新的 `order_id`；saga 异步执行。
3. **预占** —— inventory 为每行商品预占库存（状态
   `active` → `committed`）。
4. **支付** —— payment 捕获支付意向；成功后产生
   `payment.captured.v1` 事件。
5. **发货** —— fulfillment 跨仓库分单并创建发货；成功后订单标记为
   `paid` → `fulfilled`。

失败会触发补偿：支付失败会发布 `payment.failed.v1` 事件，order consumer 捕获该事件后触发
inventory 释放预占（`active` → `released`）。见 `test/smoke.sh` 中的 gate 5。

## 事件保障

事件投递是**至少一次**的。Outbox/Inbox 模式提供了使此投递安全的两项保障：

- **事务发件箱** —— 领域写操作在与状态变更相同的数据库事务中插入一条 `outbox_event` 行。
  一个轮询式 dispatcher 读取新行并发布到 RabbitMQ。这意味着一个服务永远不会在提交状态变更时
  不同时记录描述它的事件。
- **幂等收件箱** —— 每个 consumer 在应用变更之前先记录一个 `inbox_event` 键。
  相同 `event_id` 的重复投递会被检测并跳过。这是 at-least-once 投递安全的核心。

运维影响：

- 重新投递是安全的。seed CLI 和 smoke 测试都依赖此特性——
  duplicate-webhook gate（gate 4）用同一个 `Idempotency-Key` 重放两次，并断言返回的 payment id
  完全一致。
- 发件箱分发是 at-least-once 的边界。如果一个 Pod 在发布后、标记行为已分发之前崩溃，
  该行会在重启时重新发布；consumer 侧的 Inbox 负责去重。

Outbox/Inbox schema 见 `db/migrations/shared.sql`——每个服务迁移逐字包含。

## 失败注入

seed 数据确定性地产生多种订单生命周期组合，包括一部分 `payment_status=failed` 的订单。
这就是失败注入路径：无需手动 chaos engineering。

```bash
# 找一个 seed 中支付失败的订单。
curl -fsS http://localhost:18100/api/v1/orders?page_size=100 \
  | jq -r '.items[] | select(.payment_status=="failed") | .order_id' | head -n1

# 检查其失败的支付意向及失败尝试的 failure_code。
ORDER_ID=...  # 来自上一条命令
curl -fsS http://localhost:18100/api/v1/payments/orders/${ORDER_ID} | jq .

# 断言其库存预占已非 active（补偿已释放）。
curl -fsS http://localhost:18100/api/v1/reservations/${ORDER_ID} | jq .
```

seed 数据中观察到的失败码：`card_declined`、`insufficient_funds`、`risk_rejection`、
`provider_timeout`。smoke 测试的 gate 3 断言一个确定性的失败码存在。

## Broker 检查

RabbitMQ 启用了管理 UI（`rabbitmq:4.0-management`）。compose 文件将 broker 映射到内部
`commerce-data` 网络上——不发布管理端口。检查 broker：

```bash
# 在 rabbitmq 容器中打开 shell，使用 rabbitmqctl / rabbitmqadmin。
docker compose exec rabbitmq rabbitmqctl list_queues name messages consumers
docker compose exec rabbitmq rabbitmqctl list_exchanges name type
docker compose exec rabbitmq rabbitmqctl list_bindings source_name routing_key destination_name

# 或通过端口转发临时启用管理 UI（一次性，临时）：
docker compose port rabbitmq 15672   # 然后 docker run -p 15672:15672 ...（如需）
```

拓扑（对应 `deploy/rabbitmq/definitions.json`）：

- topic exchange **`commerce.events`** —— 主总线。
- topic exchange **`commerce.events.dlx`** —— 死信终点。
- direct exchange **`commerce.events.retry`** —— 支撑每队列重试模式
  （TTL 2000ms，到期后 dead-letter 回 `commerce.events`）。
- 每服务队列：`order.saga`、`payment.intents`、`fulfillment.orders`，
  各带 `.retry`（TTL + DLX）+ `.dlq` 伴侣。

## Seed 规模

seed 数据是确定性的：相同的 seed 值 + scale 产出逐字节相同的行。内置三个规模：

| Scale | 订单数 | 用途 |
|-------|------:|------|
| `dev` | 100 | 默认；完整生命周期组合含失败；适配 4–6 GB 预算 |
| `demo` | 10 000 | 演示 / 集成测试 |
| `load` | 1 000 000 | 压测；通过 `COPY FROM` 以有界内存流式灌入 |

```bash
make seed                # dev 规模，seed 42（--reset 幂等）
make verify-seed         # 校验灌入数据完整性
SEED=99 make seed        # 不同 seed → 不同的确定性数据集

# load 规模使用独立的 compose overlay，提高每副本内存，并将 order 扩到 4 副本、inventory 扩到 3 副本。
make load
```

### Load 规模资源警告：4–6 GB 预算不适用

默认 `make up` profile 面向** 4–6 GB** 内存预算。load profile（`make load`、`--scale load`）
刻意提高每服务内存（order 限制 2 GiB，Postgres shared_buffers 512 MB 等）并通过
`COPY FROM` 灌入 1 000 000 笔订单。**load 规模请按远超 4–6 GB 来规划**——具体 override 见
`compose.load.yaml`。不要在一台同时运行其他重度工作负载的笔记本上运行 load 规模。

## 可观测性

每个 Go 服务暴露：

- `/livez` —— 存活探针。进程启动后始终返回 200。
- `/readyz` —— 就绪探针。优雅关闭 drain 期间返回 503，其余时间返回 200。
  Traefik 和 Ingress controller 用它做 drain。
- `/metrics` —— Prometheus 格式指标（请求时延、发件箱分发滞后、
  收件箱去重计数、pgx 连接池统计）。

用 overlay 拉起完整可观测性栈（otel collector、Prometheus、Grafana、Jaeger）：

```bash
make observability         # 拉起 otel/prometheus/grafana/jaeger
make trace-smoke           # 断言一个 catalog 请求的 trace 到达 Jaeger
make observability-down    # 仅移除可观测性容器
```

overlay 会添加可观测性容器，覆盖应用遥测环境变量，并可能在 Compose 应用这些环境变量时重建
服务。Prometheus 通过内部网络抓取每个服务的 `/metrics`，otel collector 通过 OTLP 接收 trace。
在 `make observability` 之后运行 `make trace-smoke`；它会经网关发送一个 catalog 请求并检查
Jaeger 是否收到该请求的 trace。

## CI 门控

本地静态门控是 `make commerce-ci`。它构建并测试所有 Commerce 包，对 `internal/platform` 运行
race 检测，校验每个 Compose 配置，并将 Kubernetes kustomization 渲染到 `/tmp/commerce-k8s.yaml`
（不 apply）。

GitHub Actions 在 `commerce` job 中运行相同的检查。独立的 `commerce-e2e` job 各起一个可变服务的
副本，运行 `make smoke`，启用可观测性 overlay，并运行 `make trace-smoke`。失败时捕获合并的
Compose 日志；runner 退出前始终移除容器、卷和孤立资源。

## Kubernetes 映射

`deploy/k8s/` 中的 Kubernetes 清单为相同的应用契约提供了部署映射。它们不是 Compose 的一一对应：
集群网络、基础设施归属和滚动发布行为仍是集群特定的。

```bash
# 渲染清单但不 apply（Phase A 门控）：
kubectl kustomize deploy/k8s > /tmp/commerce-k8s.yaml

# Apply 整个 bundle（假定已安装 ingress controller）：
kubectl apply -k deploy/k8s
```

| Compose 概念 | Kubernetes 对应 | 文件 |
|-------------|----------------|------|
| project name `commerce` | Namespace `commerce` | `namespace.yaml` |
| `x-service-env` anchor | ConfigMap `commerce-shared` | `config.yaml` |
| 每服务 `environment:` | ConfigMap `<svc>-env`（每服务一个） | `config.yaml` |
| `DB_PASSWORD` / `BROKER_URL` | Secret `commerce-dev-secret`（仅 DEV，标 unsafe） | `config.yaml` |
| 服务容器 | 副本数匹配的 Deployment | `apps.yaml` |
| `healthcheck: wget /livez` | 基于 `/livez` 和 `/readyz` 的 startup/readiness/liveness 探针 | `apps.yaml` |
| `mem_reservation`/`mem_limit`/`cpus` | resource `requests`/`limits` | `apps.yaml` |
| `read_only: true` + `tmpfs:/tmp` | `readOnlyRootFilesystem: true` + `/tmp` 的 `emptyDir` Memory | `apps.yaml` |
| `security_opt: no-new-privileges` + `cap_drop: ALL` | runAsNonRoot, seccompProfile, drop ALL caps | `apps.yaml` |
| Traefik router 标签 | 路径规则匹配的 Ingress | `gateway.yaml` |
| 每服务 DNS 名 | 每应用的 ClusterIP Service | `services.yaml` |
| `*-db` service / `rabbitmq` service | ExternalName Service（非 dev 前替换） | `services.yaml` |
| `deploy.replicas: N` (N >= 2) | PodDisruptionBudget `minAvailable: 1` | `availability.yaml` |
| （水平扩缩） | 每无状态服务的 HorizontalPodAutoscaler | `availability.yaml` |

**有状态后端服务不在此重新部署。** `*-db` 和 `rabbitmq` Service 是指向 default namespace 的
ExternalName 别名——在服务任何真实流量之前，请将它们替换为你的 operator 管理的 StatefulSet
（或云托管服务）。清单刻意**不**声称运维 PostgreSQL 或 RabbitMQ 的 HA。

组件图及每个部署形态决策的依据见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 清理

```bash
make down                                  # compose：停 + 删 + 含卷
make observability-down                    # 仅可观测性 overlay

# Kubernetes（独立集群，如果你 apply 过清单）：
kubectl delete -k deploy/k8s
```

`make down` 传递 `--volumes --remove-orphans`，因此会抹除全部六个 PostgreSQL 数据卷和
RabbitMQ mnesia 卷。`make down` 后重新 `make up` 会得到干净拓扑。

## 非目标

本模板刻意**不**实现以下任何一项——它们超出范围，添加它们会模糊模板旨在展示的模式：

- **认证 / 授权。** 无 JWT、OAuth、mTLS 或 API key。所有路由刻意开放。
- **真实支付提供商。** payment 服务用确定性结果模拟 capture/refund；
  没有 Stripe/Adyen/Braintree 集成。
- **搜索。** 商品列表是 SQL `LIMIT/OFFSET` 查询，不是 Elasticsearch / OpenSearch / Algolia。
- **Redis。** 无缓存层。库存预占存在于 PostgreSQL 中，使用
  `SELECT ... FOR UPDATE` 加状态状态机。
- **促销 DSL。** 无优惠券引擎，无购物车级折扣规则。
- **PostgreSQL HA。** 每服务单副本。无流复制、无 Patroni、无自动故障切换。
  （Kubernetes 上同理——见上。）
- **RabbitMQ 集群。** 单 broker 节点。无跨多节点的 quorum queue、无 federation。
- **跨集群 federation / 多地域。** 超出范围。

如果你在真实部署中需要以上任何一项，请将本模板视为起点并在此基础上叠加——
每服务独占边界使得每一项新增都是一处局部变更。
