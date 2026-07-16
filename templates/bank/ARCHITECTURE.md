# bank 架构

## 服务拓扑（Spec B-1：3 进程 + 3 库，单 postgres 实例）

| 服务 | 端口 | 库 | cmd 入口 | 职责 |
|------|------|----|---------|------|
| core-banking | 8080 | core_db | `cmd/core-banking/main.go` | 活期/定存 + 复式记账 + 总账（Spec A 只读；B-3 新增记账/冲正写接口） |
| customer | 8081 | cust_db | `cmd/customer/main.go` | 客户信息 + 账户关系（只读，含跨库 FDW JOIN） |
| payment | 8082 | pay_db | `cmd/payment/main.go` | 转账/商户（只读，含跨库 FDW JOIN） |

每域一个独立 Go 进程，`docker-compose.yaml` 里各一个 service 定义 + 容器 + 端口。单个 postgres 实例承载 3 库，跨库查询走 `postgres_fdw`（非应用层拼接）。

## 分层（每服务独立纵切）

```
internal/
├── platform/          基础设施（pg 连接 + migration runner + fdw 联邦）
├── corebanking/       Spec A：core-banking 纵切
│   ├── domain/        纯领域模型（零 DB/框架依赖，最内层）
│   ├── repo/          仓储层（pgx raw SQL 落库）
│   ├── service/       用例层（业务规则，纯逻辑可单测）
│   └── api/           传输层（http handlers + chi router）
├── customer/          Spec B-1：customer 纵切（镜像 corebanking 四层）
│   ├── domain/
│   ├── repo/          本库 cust_info/cust_account_rel + FDW JOIN core_db
│   ├── service/
│   └── api/
├── payment/           Spec B-1：payment 纵切（镜像 corebanking 四层）
│   ├── domain/        Money int64 分（禁 float）
│   ├── repo/          本库 transfer_txn/merchant + FDW JOIN core_db + cust_db
│   ├── service/
│   └── api/
└── fixtures/          Go fixture 生成器（确定性：固定 seed）
```

依赖方向向内：`api → service → repo → domain`，`domain` 不依赖任何人。各域 `domain` 互不依赖。

## 数据库拓扑与 FDW 联邦

```
postgres 实例（容器 bank-postgres）
├── core_db   ← core-banking 连（Spec A 已有）
├── cust_db   ← customer 连（Spec B-1 新增）
└── pay_db    ← payment 连（Spec B-1 新增）
```

`postgres_fdw` 在 `cmd/seed` 末尾由 `platform/fdw.SetupFDW` 幂等建立外部表映射，命名规则 `ext_{remote}_{tbl}`：

```
core_db:  ext_cust_db_cust_info, ext_cust_db_cust_account_rel
cust_db:  ext_core_db_demand_account          ← B-1 新增映射
pay_db:   ext_core_db_demand_account, ext_cust_db_cust_info
```

外部表映射图（host 库引入 remote 库的表）：

```
core_db ←──FDW── cust_db (cust_info, cust_account_rel)
cust_db ←──FDW── core_db (demand_account)        [B-1 新增]
pay_db  ←──FDW── core_db (demand_account)
pay_db  ←──FDW── cust_db (cust_info)
```

### 2 个跨库 FDW JOIN 端点

| 端点 | 服务 | JOIN 路径 | 返回 |
|------|------|----------|------|
| `GET /api/v1/customers/{cust_id}/accounts` | customer (8081) | `cust_db.cust_account_rel` JOIN `ext_core_db_demand_account` | 客户关联的 core 账户行（账号/币种/状态/开户日） |
| `GET /api/v1/payments/transfers/{txn_id}/parties` | payment (8082) | `pay_db.transfer_txn` JOIN `ext_core_db_demand_account`(×2) JOIN `ext_cust_db_cust_info`(×2) | 转账双方账号 + 户主客户姓名 |

### core-banking 端点（:8080）

读（Spec A）+ 记账/冲正写（Spec B-3）：

| 端点 | 方法 | 说明 |
|------|------|------|
| `/api/v1/accounts/{no}` | GET | 查活期/定存账户 |
| `/api/v1/accounts/{no}/balance` | GET | 查最新 biz_date 余额 |
| `/api/v1/txns` | GET | 查流水（query: account_no/from/to） |
| `/api/v1/ledger` | GET | 查总账（query: biz_date） |
| `/api/v1/txns` | POST | 记账（deposit/withdraw/transfer）→ 复式过账，201 返回凭证+分录 |
| `/api/v1/vouchers/{voucher_no}/reverse?mode=blue\|red` | POST | 冲正（blue 默认：改状态+回滚；red：反向分录新增流水） |

## 数据流

- `cmd/seed`：连 postgres 管理库 → 建 3 库（core_db/cust_db/pay_db）→ 跑 3 套迁移 SQL → 灌 core 静态主数据/账户/余额/流水 → 灌 customer 客户+关系 → 灌 payment 商户/转账/消费 → `setup_fdw` 建外部表（6 步，幂等：`--reset` 重建）。
- `cmd/core-banking`：连 core_db，暴露 HTTP API（:8080）—— Spec A 只读查询 + B-3 记账/冲正写接口（事务内复式过账）。
- `cmd/customer`：连 cust_db（只读），暴露只读 HTTP API（:8081），含跨库 FDW JOIN。
- `cmd/payment`：连 pay_db（只读），暴露只读 HTTP API（:8082），含跨库 FDW JOIN。

## 范围

- **Spec A**：core-banking 单服务（活期/定存 + 复式记账 + 总账）。
- **Spec B-1**：customer + payment 两服务纵切 + 多库 + FDW 联邦（本架构）。
- **Spec B-3**：core-banking 新增记账/冲正写接口（`POST /txns`、`POST /vouchers/{}/reverse`）；`LedgerService.Post` 内部化为 txn_service 基础设施——客户端只见业务意图，复式分录不再对外暴露。
- **Spec B-2/B-4**（留后续）：多日切日引擎、剩余 4 服务（reward/risk/loan/wealth）。
- **注**：pay_db 的 `channel_txn`/`fee_record`/`settlement_record` 表已建（schema 完整），fixture 数据留 B-2 多日引擎一起补（B-1 不阻塞：仅 merchant/transfer 参与 FDW 联邦与只读 API）。

## 金融不变量

- 金额用 int64 分表示，禁 float（core + payment 域 Money 类型）。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚——既护 seed 灌数也护 B-3 运行时记账/冲正（事务内 Post 校验）。customer/payment 无总账。
