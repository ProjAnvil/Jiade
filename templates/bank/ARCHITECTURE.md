# bank 架构

## 服务与数据所有权

| 服务 | 端口 | 独占数据库 | 职责 |
|------|------|------------|------|
| core-banking | 18080 | core_db | 活期/定存、复式记账、余额、过账/冲正 |
| customer | 18081 | cust_db | 客户信息、账户关系 |
| payment | 18082 | pay_db | 转账、商户、消费 |
| reward | 18083 | reward_db | 积分、优惠券、活动 |
| risk | 18084 | risk_db | 风控规则、事件、黑名单 |
| loan | 18085 | loan_db | 借据、还款、逾期、余额快照 |
| wealth | 18086 | wealth_db | 产品、净值、持仓、订单、收益 |

每个服务是独立 Go 进程，只连接自己的数据库。七个数据库当前由同一个 PostgreSQL
实例承载，但不存在外部表、跨库 SQL 或共享数据库访问权限假设。

## 分层

每个业务域采用 `api → service → repo → domain` 纵切：

- `api`：HTTP handler 与路由。
- `service`：用例编排和业务规则。
- `repo`：本服务数据库访问；聚合端点所需的跨域数据通过 `platform/serviceclient`
  调用其他服务的公开 HTTP API。
- `domain`：纯领域模型，不依赖数据库或 HTTP 框架。

`platform/pg` 管理数据库连接，`platform/migrate` 执行迁移，
`platform/serviceclient` 提供带超时和状态码校验的服务间 JSON 客户端。

## 服务调用拓扑

```text
customer ────────> core-banking
payment  ────────> core-banking
payment  ────────> customer
reward   ────────> customer
risk     ────────> customer
loan     ────────> customer
wealth   ────────> customer
```

容器内通过 `CORE_BANKING_URL=http://core-banking:18080` 和
`CUSTOMER_URL=http://customer:18081` 服务发现；本地运行默认使用
`localhost:18080` / `localhost:18081`。

## 跨服务聚合端点

| 端点 | 编排 |
|------|------|
| `GET /api/v1/customers/{cust_id}/accounts` | customer 查本库关系，逐个调用 core-banking 查账户 |
| `GET /api/v1/payments/transfers/{txn_id}/parties` | payment 查本库转账，调用 core-banking 查账户归属，再调用 customer 查姓名 |
| `GET /api/v1/reward/customers/{cust_id}/profile` | reward 查本库积分，调用 customer 查客户 |
| `GET /api/v1/risk/events/{event_id}` | risk 查本库事件，调用 customer 查客户 |
| `GET /api/v1/loan/accounts/{loan_no}/profile` | loan 查本库借据，调用 customer 查客户 |
| `GET /api/v1/wealth/holdings/{holding_id}/profile` | wealth 查本库持仓，调用 customer 查客户 |

上游不可用、超时或返回非 2xx 时，聚合端点返回错误，不会回退到跨库读取。

## Seed 数据流

`cmd/seed` 只负责数据库与 fixture：

1. 创建 7 个数据库。
2. 执行 7 套迁移。
3. 按 core → customer → payment → reward → risk → loan → wealth 顺序灌数。

Seed 不安装 PostgreSQL 扩展，也不创建外部表。确定性 fixture、三因子事件流、
逐日余额/NAV 滚存逻辑保持不变。

## 金融不变量

- 金额使用 `int64` 分，禁用浮点。
- 复式记账只在 core-banking：过账强制借贷平衡，失败时整笔事务回滚。
- 跨服务查询是只读编排，不跨服务共享数据库事务。
