# bank（jiade 模板：7 服务纵切——core-banking + customer + payment + reward + risk + loan + wealth）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行（仅需 docker + go）。

本模板属于 [jiade](../../README.md) 项目；架构细节见 [ARCHITECTURE.md](ARCHITECTURE.md)。

工程包含 **7 服务 + 7 独立 PostgreSQL 库 + Traefik 网关 + RabbitMQ + 内部 gRPC + 逐日滚存/三因子 fixture**。每个服务只访问自己的数据库：

| 服务 | 容器端口 | 库 | 默认副本 | 内容 |
|------|----------|----|----------|------|
| core-banking | 8080 (REST) / 9090 (gRPC) | core_db | 2 | 活期/定存账户、复式记账总账、逐日余额、写接口（过账/冲正） |
| customer | 8080 (REST) / 9090 (gRPC) | cust_db | 1 | 客户信息、账户关系 |
| payment | 8080 (REST) | pay_db | 2 | 商户、转账、消费流水 |
| reward | 8080 (REST) | reward_db | 1 | 积分账户/流水、优惠券、活动 |
| risk | 8080 (REST) | risk_db | 2 | 风控规则、事件、黑名单 |
| loan | 8080 (REST) | loan_db | 1 | 借据、放款、月度还款、五级分类逾期、**逐日余额快照** |
| wealth | 8080 (REST) | wealth_db | 1 | 理财产品、**逐日净值游走**、持仓、申赎订单、每日利息 |

**网关**：Traefik 是唯一对外发布的端口——`http://localhost:18000`。所有公开 REST 路径（`/api/v1/...`）经网关路由到对应服务的 8080 端口；`/internal/*` 与 gRPC 9090 不暴露给宿主机。

**服务间通信**：同步只读查询走内部 gRPC（customer、core-banking 在 :9090 提供 `CustomerQueryService` / `AccountQueryService`）；异步命令与领域事件走 RabbitMQ（事务发件箱 + 有限重试 + DLQ）。详见 [ARCHITECTURE.md](ARCHITECTURE.md)。

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

**跨服务聚合端点**（服务经内部 gRPC 协作，不跨库查询）：

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

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。7 进程 + 7 独立 PostgreSQL + RabbitMQ + Traefik 网关；同步跨域读取走内部 gRPC，异步命令/事件走 RabbitMQ；每服务分层 `api → service → repo → domain`，domain 零外部依赖。

## 运行拓扑

- **网关**：Traefik v3 是唯一发布到宿主机的端口（`18000:8080`）；只路由公开 `/api/v1/...` REST 前缀。
- **专用数据库**：七个 PostgreSQL 16 实例（`core-banking-db` … `wealth-db`），各自独立卷，不存在跨库 SQL / 外部表 / 共享库权限。
- **消息中间件**：RabbitMQ 4（`bank.commands` / `bank.events` / `bank.retry` / `bank.dlx` 四个交换机），命令队列各带 `.retry`（2 秒 TTL）+ `.dlq` 伴侣。
- **内部 gRPC**：`customer:9090` 与 `core-banking:9090` 对内暴露只读查询；其余五服务仅起 REST，作为 gRPC 消费方。
- **副本默认**：core-banking / payment / risk = 2；customer / reward / loan / wealth = 1。
- **本地资源预算**：全栈约 6–8 GB 内存。应用容器限 512 MB / 1 CPU，PostgreSQL 限 768 MB，RabbitMQ 限 768 MB，Traefik 限 256 MB。
- **弹性扩缩**：`make scale SERVICE=payment REPLICAS=3`（仅扩缩指定服务，不动依赖）。
- **安全基线**：应用容器 `read_only` + `tmpfs:/tmp` + `cap_drop: ALL` + `no-new-privileges`。

### Kubernetes 基线（`deploy/k8s/base`）

`kubectl kustomize deploy/k8s/base` 可渲染出**纯应用层**清单：7 Deployment + 14 Service（7 REST ClusterIP + 7 headless gRPC）+ 1 Ingress（仅公开 REST）+ 3 PDB（core-banking/payment/risk）+ 7 HPA（CPU 驱动）+ 1 ConfigMap（非密默认值）。

**此基线不包含任何有状态资源**——没有 StatefulSet、PV/PVC、Secret、PostgreSQL、RabbitMQ。 runnable 运行态依赖由后续 operational-closure overlay 注入。本地 Compose 拓扑亦**不声称有状态高可用**：PostgreSQL 与 RabbitMQ 各为单副本，仅用于开发与集成测试。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚——既护 seed 也护 B-3 运行时记账/冲正。customer/payment 无总账。
