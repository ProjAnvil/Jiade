# Jiade — Spec B-1 设计文档

- **日期**：2026-07-15
- **状态**：设计待审（待用户复审）
- **范围**：Spec B-1 —— `customer` + `payment` 服务纵切 + 多库 + FDW 跨库联邦
- **作者**：yuhaochen × Claude（brainstorming）

---

## 1. 背景与定位

### 1.1 Spec B 的分解

Spec B（Spec A §12 预告）原本是一整块：补齐 bank 其余 6 服务 + 多库 FDW 联邦 + 多日切日引擎 + 写操作 HTTP 接口。范围过大，需分解为可独立交付的子 spec。依赖关系是分解的依据：

| 子 spec | 范围 | 依赖 | 体量 |
|---------|------|------|------|
| **B-1** | customer + payment 两服务（schema/fixture/只读 API）+ 建立多库 + FDW 联邦模式 | Spec A | 中 |
| **B-3** | 写操作 HTTP 接口（过账/记账/冲正，暴露已有 `LedgerService.Post`） | Spec A service 层 | 小 |
| **B-2** | 多日切日引擎（`bizdate`+`distribution` 移植，替换 Spec A 简化 fixture） | Spec A core_db | 中 |
| **B-4** | 剩余 4 服务（reward/risk/loan/wealth），复制 B-1 模式 | **B-1**（多库模式 + customer 数据） | 大（模式重复） |

> **关键**：B-3、B-2 都只依赖 Spec A，与 B-1 **互相独立**；只有 B-4 依赖 B-1（复用多库 + FDW 模式，并以 customer 为数据依赖源）。

**执行顺序**：B-1 → B-3 → B-2 → B-4（B-3 小而独立，B-1 后趁手做，更早落一个完整交付物）。

### 1.2 B-1 的位置

B-1 是 Spec B 的**首发纵切**，目标是把「每加一个服务 = 加一个库 + FDW 联邦视图」这个 Spec A §2.2 预留的扩展模式**一次性建立并验证**。一旦 B-1 把多库 + FDW + 多服务进程 + 跨域确定性关联跑通，后续 B-4 只是复制模式填服务。

### 1.3 与 Spec A 的关系

**填内容，不改架构**。Spec A 的模板契约、每服务目录布局（`cmd/` 多入口 + `internal/<域>/` 分层）、compose 结构、fixture 确定性范式全部沿用。B-1 是在 Spec A 的 core-banking 纵切旁边，**平行加两条纵切**（customer、payment），并把单库基础设施泛化为多库。

---

## 2. 目标与非目标

### 2.1 B-1 目标

1. **customer 服务**（cust_db）：客户域 5 表 schema + 确定性 fixture + 完整四层（domain/repo/service/api）只读 API。
2. **payment 服务**（pay_db）：支付域 6 表 schema + 确定性 fixture + 完整四层只读 API；金额 int64 分。
3. **多库基础设施**：单 postgres 实例 + 3 库（core_db/cust_db/pay_db），`ensureDB`/`migrate`/`template.yaml`/`compose` 从单库泛化为多库。
4. **FDW 跨库联邦**：`setup_fdw` 幂等建立外部表映射；customer、payment **各至少 1 个跨库 FDW JOIN 只读端点**，真正验证联邦查询模式。
5. **多进程微服务部署**：每域一个独立 Go 进程 + compose 容器 + 端口。
6. **测试与验收**：确定性、service 单测、repo+api 集成、FDW 联邦集成、e2e 冒烟。

### 2.2 B-1 非目标（留给 B-2/B-3/B-4 或以后）

- **写操作 HTTP 接口**（过账/记账/冲正）—— Spec A service 层已实现 + 单测，B-3 暴露 HTTP。B-1 全程**只读 API**。
- **多日切日引擎**（`bizdate`/`distribution`）—— B-2。B-1 的 fixture 是「一次性快照式」生成（biz_date 散布在范围内，非逐日滚存）。
- **loan/wealth 的多日数据**（`loan_balance`/`wealth_nav` 等 biz_date 维度）—— 随 B-4 各域。
- **reward/risk/loan/wealth 四服务** —— B-4。
- **core-banking 服务代码改动** —— B-1 不改 core-banking（保持 Spec A 稳定）；core_db 仅新增 cust_db 的 FDW 外部表（映射完整性）。
- **元数据/指标层** —— 不在 Jiade 范围（Jiade 自闭，无 BI 指标层）。

---

## 3. 核心原则（延续 Spec A）

| 原则 | 在 B-1 的体现 |
|------|---------------|
| **生成物自包含** | 拷出的工程离开 `jiade` 也能 `docker compose up`（3 服务）+ `go run ./cmd/seed`。 |
| **自闭** | jiade 不联动 SCV/Porto；B-1 只填充 bank 模板内容。 |
| **缩影哲学** | 保留模式（多库 + FDW 联邦 + 微服务 + 分层 + 金融不变量），砍规模（服务数 7、表 41、数据量 dev 级）。 |
| **金额 int64 分，禁 float** | payment 域金额字段全程 int64 分 + repo 分↔NUMERIC 转换，对齐 core。 |
| **复式记账只在 core** | 写接口（B-3）经 `LedgerService.Post`；customer/payment 无总账，service 层只做查询编排。 |
| **依赖方向向内** | 各服务 `api → service → repo → domain`；各域 domain 互不依赖。 |
| **确定性** | 同 Seed+Scale → 同样的行；跨域确定性关联（编号规则一致）。 |

---

## 4. 架构总览

### 4.1 服务拓扑（3 进程 + 3 库，单 postgres 实例）

| 服务 | 端口 | 库 | cmd 入口 | 状态 |
|------|------|----|---------|------|
| core-banking | 8080 | core_db | `cmd/core-banking/main.go` | 已有（Spec A） |
| customer | 8081 | cust_db | `cmd/customer/main.go` | 新 |
| payment | 8082 | pay_db | `cmd/payment/main.go` | 新 |

每域一个独立 Go 进程，compose 里各一个 service 定义 + 容器 + 端口。单 postgres 实例承载 3 库，跨库查询走 postgres_fdw（非应用层拼接）。

### 4.2 数据库拓扑

```
postgres 实例（容器 bank-postgres）
├── core_db   ← core-banking 连（Spec A 已有）
├── cust_db   ← customer 连（新）
└── pay_db    ← payment 连（新）

FDW 外部表（seed 末尾 setup_fdw 建立，命名 ext_{remote}_{tbl}）：
├── core_db:  ext_cust_db_cust_info, ext_cust_db_cust_account_rel
├── cust_db:  ext_core_db_demand_account          ← B-1 新增映射
└── pay_db:   ext_core_db_demand_account, ext_cust_db_cust_info
```

### 4.3 数据流

```
jiade init --template bank --dir ./mybank
   └─ copy templates/bank/. → ./mybank/（含 customer/payment 两服务源码）
jiade up   └─ docker compose up -d（postgres + core-banking + customer + payment）
jiade seed └─ go run ./cmd/seed --scale=dev --reset
              （建 3 库 → 建 3 库表 → core → customer → payment → setup_fdw，幂等）
curl localhost:8081/api/v1/customers/{id}/accounts        # 跨库 FDW JOIN
curl localhost:8082/api/v1/payments/transfers/{id}/parties # 跨库 FDW JOIN
```

---

## 5. 多库基础设施泛化

### 5.1 建库与建表

Spec A 的 `ensureDB`（只建 core_db）泛化为**按库名循环**：对 `core_db`/`cust_db`/`pay_db` 依次「不存在则建；`--reset` 则 terminate 连接 + DROP + CREATE」。`pg.Open(name)` 已支持任意库名，无需改动。

`migrate.Run` 不变（按分号切 DDL，Spec A §13 已知限制：DDL 无嵌套分号；cust_db.sql/pay_db.sql 同样是无函数体的纯 CREATE TABLE，安全）。`seed/main.go` 对每个库读 `db/migrations/<db>.sql` 并 `migrate.Run`。

### 5.2 模板契约与 compose

`template.yaml`：

```yaml
name: bank
description: 简化版银行核心系统（core/customer/payment 服务，Spec B-1）
version: 0.2.0
databases:
  - name: core_db
    migrate: db/migrations/core_db.sql
  - name: cust_db
    migrate: db/migrations/cust_db.sql
  - name: pay_db
    migrate: db/migrations/pay_db.sql
services:
  - {name: core-banking, port: 8080, db: core_db}
  - {name: customer, port: 8081, db: cust_db}
  - {name: payment, port: 8082, db: pay_db}
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

`docker-compose.yaml`：在 Spec A 基础上加 `customer`、`payment` 两个 service（各自 `build: .`、`DB_NAME`、`API_PORT`、端口映射、`depends_on: postgres.service_healthy`）。沿用 Spec A 的 `restart: unless-stopped` + Makefile 两阶段 up（postgres→seed→服务）模式，扩展为 seed 后起 3 服务。

---

## 6. fixture 编排扩展

### 6.1 编排顺序

`seed/main.go` 从 Spec A 的 4 步扩为：

```
1. 建 3 库（ensureDB 循环）
2. 建 3 库表（各库 migrate.Run 对应 .sql）
3. core 域（Spec A 已有：static / accounts / balances / txns）
4. customer 域（cust_info / cust_id_doc / cust_contact / cust_org / cust_account_rel）
5. payment 域（merchant / transfer_txn / consumption_txn / channel_txn / fee_record / settlement_record）
6. setup_fdw（在各库建 FDW 外部表映射）
```

### 6.2 确定性跨域关联（零破坏 Spec A）

Spec A 的 core 自生成确定性编号（`internal/fixtures/domains/core.go`）：

- `nCustomers = DemandAccounts / 2`（dev = 1000）
- `demand[i].AccountNo = D%010d(i+1)`
- `demand[i].CustID  = C%07d((i % nCustomers) + 1)`
- `fixed[i].AccountNo = F%010d(i+1)`，`CustID` 同上

**B-1 不重构 core**。customer 域用**相同的编号规则**独立生成，从而确定性关联：

- 生成 `nCustomers` 个 `cust_info`，`cust_id = C%07d(j+1)`，`j ∈ [0, nCustomers)` → 覆盖 core 所有 `account.cust_id`。
- `cust_account_rel`：遍历 core 的 demand accounts，每条 `{cust_id: account.cust_id, account_no: account.account_no, role: "主", rel_type: "户主"}`（缩影：仅户主关系）。

因编号规则确定性一致，customer 与 core **无需运行时传参即可关联**；但为保持编排显式（与 Spec A 风格一致），`seed/main.go` 仍把 core 生成的 `demandNos`/`custIDs` 显式传入 customer/payment 生成器。

### 6.3 customer 域生成

- `cust_info`：`nCustomers` 行；`cust_id = C%07d(j+1)`；姓名/证件类型/证件号/性别/生日/国籍/风险等级/kyc_status/客户类型（个人/对公）从扩展词库（`rng.go` 加姓名/证件/kyc 词库）确定性生成；`create_biz_date` 散布在 `[start, end]` 范围。
- `cust_id_doc` / `cust_contact` / `cust_org`：保留客户域 schema 完整性，确定性生成少量行（对公客户才有 `cust_org`）。
- `cust_account_rel`：见 §6.2。
- rng 偏移：`cfg.Seed + 10`（避开 core 的 `+1/+2/+3`）。

### 6.4 payment 域生成

- `merchant`：`M%05d(k+1)`，`k ∈ [0, nMerchants)`（dev 缩影 ~50）；名称/mcc/region 从词库。
- `transfer_txn`：`txn_id = PT%012d(seq)`；`out_account`/`in_account` 从 core 的 `demandNos` 随机选；`amount` int64 分；`biz_date` 散布在范围。
- `consumption_txn`：`account_no`（core）+ `merchant_id`；金额 int64 分。
- `channel_txn` / `fee_record` / `settlement_record`：保留支付域完整性，确定性生成少量行。
- rng 偏移：`cfg.Seed + 20`。
- 量级策略：B-1 **不做切日滚存**（B-2 才做），txn 是一次性快照式批量生成，总量级缩小到 dev 级（具体常数 plan 阶段定）。

### 6.5 域间独立性

customer/payment 的 txn 与 core 的 `acct_txn` **独立生成、不强求账务一致**——各域独立生成，是缩影的合理简化（真实一致性属写接口 B-3 的范畴）。

---

## 7. FDW 跨库联邦

### 7.1 FDW 映射（含 B-1 扩展）

原型映射中与 core/cust/pay 相关的三条全保留：

| host_db | remote_db | tables | 来源 |
|---------|-----------|--------|------|
| core_db | cust_db | cust_info, cust_account_rel | 原型 |
| pay_db | core_db | demand_account | 原型 |
| pay_db | cust_db | cust_info | 原型 |
| **cust_db** | **core_db** | **demand_account** | **B-1 新增**（让 customer 服务能联邦查账户） |

（loan/wealth/reward 的 FDW 映射留 B-4。）

### 7.2 setup_fdw 步骤

`internal/platform/fdw/fdw.go`（新包），幂等：

```
对每条 (host_db, remote_db, tables):
  连 host_db（AUTOCOMMIT）
  CREATE EXTENSION IF NOT EXISTS postgres_fdw
  server = fdw_{remote_db}
  DROP SERVER IF EXISTS {server} CASCADE
  CREATE SERVER {server} FOREIGN DATA WRAPPER postgres_fdw
    OPTIONS (host 'localhost', port '5432', dbname '{remote_db}')
  DROP USER MAPPING IF EXISTS FOR CURRENT_USER SERVER {server}
  CREATE USER MAPPING FOR CURRENT_USER SERVER {server}
    OPTIONS (user 'bank', password 'bank')
  对每个 tbl:
    DROP FOREIGN TABLE IF EXISTS ext_{remote_db}_{tbl}
    DROP FOREIGN TABLE IF EXISTS {tbl}          # 防御残留
    IMPORT FOREIGN SCHEMA public LIMIT TO ({tbl}) FROM SERVER {server} INTO public
    ALTER FOREIGN TABLE {tbl} RENAME TO ext_{remote_db}_{tbl}
```

`seed/main.go` 第 6 步调用。幂等保证 `--reset` 重跑安全。

### 7.3 FDW server host 技术要点

联邦对象是**同一 postgres 实例**的其他库。FDW server 由 pg 进程发起连接，故 host 统一设 `localhost`（pg 进程视角连自己），port `5432`，user/password `bank`/`bank`（compose env 一致）。无论 seed 从 host 跑（`go run`，经 `localhost:5432` 映射）还是服务从容器跑（经 `postgres:5432`），pg 进程连自己 `localhost` 永远成立——FDW server host 与外部连接方式解耦。

### 7.4 联邦查询端点

- **customer** `GET /api/v1/customers/{cust_id}/accounts`：在 cust_db 执行
  `cust_account_rel`(本地) JOIN `ext_core_db_demand_account`(FDW) → 该客户的活期账户（account_no/ccy/status/open_biz_date/branch）。
- **payment** `GET /api/v1/payments/transfers/{txn_id}/parties`：在 pay_db 执行
  `transfer_txn`(本地) JOIN `ext_core_db_demand_account`(FDW，out/in) JOIN `ext_cust_db_cust_info`(FDW) → 双方账户 + 户主姓名。

这俩端点真正发起跨库 JOIN，验证「本域为主 + FDW 关联他域」的联邦模式。

---

## 8. 服务纵切（customer / payment）

### 8.1 分层结构（对齐 Spec A §7.1）

每个新服务独立一套，依赖方向向内 `api → service → repo → domain`：

```
internal/
├── customer/            # 新
│   ├── domain/          # Customer / AccountRel（纯模型，零依赖）
│   ├── repo/            # pgx 落库 + 跨库 FDW JOIN 查询
│   ├── service/         # 查询编排（纯逻辑，可单测）
│   └── api/             # http handlers + chi router
├── payment/             # 新
│   ├── domain/          # Transfer / Consumption / Merchant + Money（int64 分）
│   ├── repo/
│   ├── service/
│   └── api/
└── corebanking/         # Spec A，不动
```

`cmd/customer/main.go`、`cmd/payment/main.go`：依赖装配（pg 连自己的库 → repo → service → api → router → 起 server），参照 `cmd/core-banking/main.go`。

### 8.2 customer 服务

- **domain**：`Customer{CustID, CustType, Name, CertType, CertNo, Gender, Birthday, Nationality, RiskLevel, KYCStatus, CreateBizDate}`、`AccountRel`。无金额字段，不需要 Money。
- **service**：查询编排（按 cust_id 查、列表筛选、联邦查账户）。薄，纯逻辑可单测。
- **repo**：cust_info CRUD + 跨库 FDW JOIN（`/customers/{id}/accounts`）。
- **api**：见 §9。

### 8.3 payment 服务

- **domain**：`Transfer`、`Consumption`、`Merchant`、`ChannelTxn`、`FeeRecord`、`Settlement`。
- **service**：查询编排。
- **repo**：本库 txn 查询 + 跨库 FDW JOIN（`/transfers/{id}/parties`）。
- **api**：见 §9。

### 8.4 金额 int64 分（payment domain）

payment domain 定义**自己的 `Money` 类型**（int64 分，禁 float，`String()` 输出 NUMERIC 文本，与 core `domain.Money` 同构），以保持 payment 服务 domain 包**独立自包含**（离开 core-banking 也能编译）。repo 做分↔NUMERIC 转换，对齐 Spec A `txn_repo.go` 范式。customer domain 无金额，不需要 Money。

### 8.5 schema 全建、API 聚焦

cust_db（5 表）、pay_db（6 表）的 schema **全部忠实还原**（保留客户域/支付域模式），fixture **全部灌数据**（保留完整性）。但 API **聚焦核心查询 + 1 联邦端点**，非核心表（如 `cust_id_doc`/`channel_txn`/`settlement_record`）数据存在但 B-1 不强制暴露端点——缩影聚焦，避免 API 面过度膨胀。

---

## 9. 只读 API（端点清单）

**customer 服务** (:8081 / cust_db)
- `GET /healthz`
- `GET /api/v1/customers/{cust_id}` — cust_info 详情（本库）
- `GET /api/v1/customers?type=&kyc_status=&offset=&limit=` — 列表（本库）
- `GET /api/v1/customers/{cust_id}/accounts` — 🔗 **联邦**：cust_account_rel JOIN ext_core_db_demand_account

**payment 服务** (:8082 / pay_db)
- `GET /healthz`
- `GET /api/v1/payments/transfers?account_no=&from=&to=&offset=&limit=` — 转账列表（本库）
- `GET /api/v1/payments/transfers/{txn_id}` — 转账详情（本库）
- `GET /api/v1/payments/transfers/{txn_id}/parties` — 🔗 **联邦**：transfer_txn JOIN ext_core_db_demand_account + ext_cust_db_cust_info
- `GET /api/v1/merchants/{merchant_id}` — 商户详情（本库）

写操作（过账/记账/冲正）不在 B-1（B-3）。

---

## 10. 测试策略（三层 + FDW + e2e，对齐 Spec A §9）

1. **fixture 确定性**：customer/payment 域，同 Seed+Scale → 同样的行，哈希比对（扩展 Spec A 的确定性单测）。
2. **service 层纯逻辑单测**：customer/payment 查询编排（联邦 JOIN 的 SQL 构造可在 service/repo 边界单测）。
3. **repo + api 集成**：真 pg，各服务连自己的库，断言本库查询 + 联邦 JOIN 查询返回正确。
4. **FDW 联邦集成测试**：`setup_fdw` 后，断言 2 个联邦端点能 JOIN 到跨库数据（customer 端点查到 core 账户；payment 端点查到 core 账户 + cust 客户姓名）。
5. **e2e 冒烟**：Makefile/CI `jiade init → up（3 服务）→ seed → curl` 三服务 `/healthz` + 2 联邦端点。

---

## 11. 错误处理（对齐 Spec A §10）

- **FDW 查询失败**（外部表未建 / server 缺失）：透出 pg 原始错误，非零退出或 5xx；e2e/集成测试保证 `setup_fdw` 先于服务查询。
- **建库/建表失败**（pg 没起）：`ensureDB` 短暂重试后透出「请先 make up」。
- **多库 partial 失败**：seed 编排中任一库失败即整体失败 + 非零退出，stderr 透传。
- **端口冲突**：compose 固定 8080/8081/8082/5432；冲突由 compose 自身报错，`.env.example` 写明改法。
- **`--reset`**：仍是所有破坏性操作的显式确认（DROP 3 库重建）。

---

## 12. 验收标准

1. jiade 仓自身：`go build ./...` 通过、`go test ./...` 全绿（不含 `templates/`）。
2. `templates/bank/` 独立 module：拷到临时目录后 `go build ./... && go test ./...` 全绿（CI 单独步骤）。
3. `jiade init --template bank` 产出含 customer/payment 服务（compose 3 Go 服务定义、template.yaml 3 库 3 服务）。
4. `cd /tmp/mybank && jiade up && jiade seed` 后：curl `core:8080`/`customer:8081`/`payment:8082` 的 `/healthz` 均 200。
5. 2 个联邦端点返回跨库数据：`GET /customers/{id}/accounts` 有账户行；`GET /transfers/{id}/parties` 有客户姓名。
6. 同 Seed+Scale 两次 seed 产出完全一致（customer/payment 域确定性单测通过）。
7. 生成物自包含：未安装 jiade（仅 docker+go）下也能 `docker compose up`（3 服务）+ `go run ./cmd/seed`。

---

## 13. 后续子 spec 预告

- **B-3（写 HTTP 接口）**：暴露 Spec A 的 `LedgerService.Post` —— 过账/记账/冲正 HTTP 接口；借必等于贷不变量在 HTTP 层生效。小而独立。
- **B-2（多日切日引擎）**：`bizdate`（逐日推进 acct_txn/account_balance + 切 sys_param.biz_date）+ `distribution`（trend/seasonal/cyclical 三因子 + 每日独立 rng）。**范围限定 core_db**（loan/wealth 多日数据随 B-4）。
- **B-4（剩余 4 服务）**：reward/risk/loan/wealth，复制 B-1 的多库 + FDW + 四层只读模式；customer 已就绪作为数据依赖源。

---

## 14. 必须延续的 Spec A 约束

- **自闭**：jiade 不联动 SCV/Porto，无 doctor。
- **生成物自包含**：bank 工程离开 jiade 可 `docker compose up` + `go run`。
- **金额 int64 分，禁 float**（payment 域 Money + repo 分↔NUMERIC）。
- **复式记账只在 core**：customer/payment 无总账；写接口（B-3）经 `LedgerService.Post`。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；各域 domain 互不依赖。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改后须 `go generate ./internal/template` 重新打包 `templates.tar`（Makefile test/e2e 已依赖 generate）。
- **go 1.22**：bank module 的 `go.mod` 须 pin `1.22`（`go mod init` 写本地版本须手动改）；本地验证 macOS 15 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。

---

## 15. 开放问题

- 无待决问题。B-1 的拓扑（多进程微服务）、纵切深度（customer/payment 完整四层只读）、FDW 演示深度（选项 1：两服务各 1 跨库 JOIN 端点 + 扩展 cust_db←core_db 映射）、执行顺序（B-1→B-3→B-2→B-4）、B-2 范围限定均已确认。
