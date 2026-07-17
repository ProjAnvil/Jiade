# Jiade — Spec B-4a 设计文档

- **日期**：2026-07-16
- **状态**：设计待审（待用户复审）
- **范围**：Spec B-4a —— `reward` + `risk` 两服务纵切（schema / 确定性 fixture / 四层只读 API / FDW 跨库联邦）
- **作者**：yuhaochen × Claude（brainstorming）
- **上游**：B-1（多库 + FDW + 四层模式）、B-2（distribution 三因子 + 逐日滚存内核）

---

## 1. 背景与定位

### 1.1 B-4 的再分解

Spec B-4 原本覆盖剩余 4 服务（reward/risk/loan/wealth），是 Spec B 里**最大**的一块（4 × B-1 体量，模式重复）。为保持每轮 brainstorm → plan → 实现可聚焦、可独立验收，B-4 按复杂度自然缝再分为两个独立子 spec：

| 子 spec | 范围 | 特征 | 依赖 |
|---------|------|------|------|
| **B-4a** | reward + risk | 三因子事件流、无逐日余额快照、表少（6+3） | B-1（多库+FDW+四层）、B-2（三因子） |
| **B-4b** | loan + wealth | 逐日余额/净值快照 + 状态滚存、表多（6+5） | B-1、B-2（滚存形态） |

本文档只覆盖 **B-4a**。B-4b 作为下一个独立单元，本次不实现、不动其 schema/服务/库。

### 1.2 B-4a 的位置

把 B-1 建立的「每加一个服务 = 加一个库 + 四层只读 + FDW 联邦」模式，**再复制两条纵切**（reward、risk），并把 B-2 落地的 distribution 三因子**首次复用到 core 之外的域**——验证这套逐日事件生成范式可横向迁移（为 B-4b 的 loan/wealth 滚存铺路）。

### 1.3 与 B-1 / B-2 的关系

**填内容，不改架构**。B-1 的多库基础设施（`ensureDBs`/`migrate`/compose/template.yaml）、FDW 装配（`fdw.SetupFDW`）、四层骨架、确定性关联范式全部沿用；B-2 的 `trendFactor/seasonalFactor/cyclicalFactor` + `bizDateRange` + 批量写入范式被 reward/risk 的事件表生成**同包直接复用**。core/customer/payment/bizdate.go 代码**不重构、不改动**。

---

## 2. 目标与非目标

### 2.1 B-4a 目标

1. **reward 服务**（reward_db）：权益域 6 表 schema（忠实还原）+ 确定性 fixture（静态一次性 + 事件逐日三因子）+ 完整四层只读 API + 1 联邦端点。
2. **risk 服务**（risk_db）：风控域 3 表 schema + 确定性 fixture + 完整四层只读 API + 1 联邦端点。
3. **多库扩展**：postgres 实例从 3 库扩到 5 库（+reward_db/risk_db）；compose/template.yaml 加 2 服务 2 端口。
4. **FDW 联邦扩展**：`Mappings` 加 reward_db/risk_db ← cust_db.cust_info；两服务各 1 跨库 JOIN 端点。
5. **B-2 三因子横向复用**：reward/risk 事件表逐日生成复用 B-2 factor + 每日独立 rng，验证范式可迁移。
6. **测试与验收**：确定性（含周末日均 < 工作日）、service 单测、repo+api 集成、FDW 联邦集成、e2e 冒烟。

### 2.2 B-4a 非目标（→ B-4b 或以后）

- **loan / wealth 两服务**及 `loan_balance`/`wealth_nav` 逐日余额/净值快照 → B-4b。
- **写操作 HTTP 接口** → core 已由 B-3 覆盖；reward/risk 无总账，不做写接口。
- **reward/risk 与 core 账务的真实一致性** → 缩影合理简化，各域独立生成（对齐 B-1 §6.5）。
- **`coupon_usage` 端点** → 表建全、本次无数据无端点（对齐 B-1 对 `channel_txn`/`settlement_record` 的处理）。
- **`meta` 指标层** → 不在 Jiade 范围（自闭，无 BI 指标层）。
- **重构既有代码** → core/customer/payment/bizdate.go 不动。

---

## 3. 核心原则（延续 Spec A / B-1）

| 原则 | 在 B-4a 的体现 |
|------|---------------|
| **生成物自包含** | 拷出的工程离开 jiade 也能 `docker compose up`（5 服务）+ `go run ./cmd/seed`。 |
| **自闭** | jiade 不联动 SCV/Porto；B-4a 只填充 bank 模板内容。 |
| **缩影哲学** | 保留模式（多库 + FDW + 微服务 + 分层 + 逐日三因子），砍规模（dev 级行数）。 |
| **金额 int64 分，禁 float** | reward coupon 金额字段 int64 分（自带 Money）；risk 无金额字段。 |
| **复式记账只在 core** | reward/risk 无总账；service 层只做查询编排。 |
| **依赖方向向内** | 各服务 `api → service → repo → domain`；各域 domain 互不依赖。 |
| **确定性** | 同 Seed+Scale → 同样的行；每日独立 rng 使单日可复现；跨域编号规则一致关联。 |

---

## 4. 架构总览

### 4.1 服务拓扑（5 进程 + 5 库，单 postgres 实例）

| 服务 | 端口 | 库 | cmd 入口 | 状态 |
|------|------|----|---------|------|
| core-banking | 8080 | core_db | `cmd/core-banking/main.go` | 已有 |
| customer | 8081 | cust_db | `cmd/customer/main.go` | 已有（B-1） |
| payment | 8082 | pay_db | `cmd/payment/main.go` | 已有（B-1） |
| **reward** | **8083** | **reward_db** | `cmd/reward/main.go` | **新** |
| **risk** | **8084** | **risk_db** | `cmd/risk/main.go` | **新** |

每域一个独立 Go 进程，compose 里各一个 service 定义 + 容器 + 端口，沿用 B-1 的 `&svcenv` 锚点 + `restart: unless-stopped` + `depends_on: postgres.service_healthy`。

### 4.2 数据库拓扑

```
postgres 实例（容器 bank-postgres）
├── core_db    ← core-banking（已有）
├── cust_db    ← customer（已有，B-1）
├── pay_db     ← payment（已有，B-1）
├── reward_db  ← reward（新）
└── risk_db    ← risk（新）

FDW 外部表（seed 末尾 setup_fdw 建立，命名 ext_{remote}_{tbl}）：
├── reward_db: ext_cust_db_cust_info          ← B-4a 新增
└── risk_db:   ext_cust_db_cust_info          ← B-4a 新增
```

### 4.3 数据流

```
jiade init --template bank --dir ./mybank
   └─ copy templates/bank/. → ./mybank/（含 reward/risk 两服务源码）
jiade up   └─ docker compose up -d（postgres + 5 服务）
jiade seed └─ go run ./cmd/seed --scale=dev --reset
              （建 5 库 → 建 5 库表 → core → customer → payment → reward → risk → setup_fdw）
curl localhost:8083/api/v1/reward/customers/{id}/profile   # 跨库 FDW JOIN
curl localhost:8084/api/v1/risk/events/{event_id}          # 跨库 FDW JOIN
```

---

## 5. schema（忠实还原）

### 5.1 reward_db（6 表）

`points_acct` / `points_txn` / `coupon` / `coupon_usage` / `campaign` / `member_level`。DDL 列定义固定（纯 CREATE TABLE，`migrate.Run` 安全）。金额字段（`coupon.face_value`/`min_spend`、`coupon_usage.deduct_amount`）为 `NUMERIC(18,2)`；积分字段（`points_balance`/`frozen_points`/`points`）为 `INTEGER`。

### 5.2 risk_db（3 表）

`risk_rule` / `risk_event` / `blacklist`。DDL 列定义固定。`risk_score NUMERIC(6,2)`（0–1 评分，非金额）、`threshold NUMERIC(18,2)`（通用阈值，非金额）——均按 NUMERIC 文本直存，不引入 Money。

### 5.3 数据策略（建表全、数据聚焦）

| 表 | 数据 | 端点 |
|----|------|------|
| member_level | 静态全量（5 档） | —（被 profile 联邦带出） |
| campaign | 静态（`12×sf` 个） | — |
| points_acct | 每客户 1 行初始 | 详情 / 列表 |
| points_txn | **逐日三因子** | — |
| coupon | **逐日**（每笔 points_txn 5% 概率发券） | 客户优惠券列表 |
| coupon_usage | **有表无数据无端点** | — |
| risk_rule | 静态全量（5 条） | 规则列表 |
| blacklist | 静态（`20×sf` 条） | 黑名单列表 |
| risk_event | **逐日三因子** | 详情（联邦）/ 列表 |

---

## 6. fixture 生成（逐日三因子，复用 B-2 内核）

### 6.1 文件落点与复用

新增 `templates/bank/internal/fixtures/domains/reward.go`、`risk.go`，**与 `bizdate.go` 同 `domains` 包**，直接调用其未导出的 `trendFactor(d)`/`seasonalFactor(d)`/`cyclicalFactor(d)`/`bizDateRange(start,end)`，零重构。批量写入复用 `placeholders(nRows,nCols)` + `pg.RunInTx` 范式（参照 `bizdate.go` 的 `bulkInsertTxns`）。

### 6.2 scale_factor helper

`fixtures` 包新增导出函数：

```go
// ScaleFactor 返回规模缩放（dev=0.25, full=1.0），reward/risk/loan/wealth 共用。
func ScaleFactor(s Scale) float64 {
    if s == ScaleFull { return 1.0 }
    return 0.25
}
```

reward/risk 的每日量 = `base × ScaleFactor(cfg.Scale) × factor`。

### 6.3 reward 域生成

- **静态**（`WriteRewardStatic(ctx,db,cfg,custIDs)`，幂等 DELETE→INSERT）：
  - `member_level`：5 档（L1 普通 0 / L2 银卡 10000 / L3 金卡 50000 / L4 白金 200000 / L5 钻石 1000000），`benefits_json` 文本。
  - `campaign`：`n = max(3, int(12×sf))` 个；name/type/start~end/budget 确定性生成（rng `seed+30`）。
  - `points_acct`：每客户 1 行；`points_balance` 随机 0–5000；`member_level` 随机档；`update_biz_date = StartBizDate`（rng `seed+30`）。
- **逐日**（`RunReward(ctx,db,cfg,custIDs)`）：`bizDateRange` 逐日，每日独立 rng（`seed+31+ordinal`），`pg.RunInTx` 内 `DELETE points_txn/coupon WHERE biz_date=$1` + 批量 INSERT：
  - `factor = trend×seasonal×cyclical`；`n_txn = max(1, int(50×sf×factor))`。
  - 每笔：随机客户；direction earn:redeem≈3:1；积分 10–500；earn 则 `balances[cid]+=pts`，redeem 则 `balances[cid]=max(0, -pts)`（内存滚存，写回 points_acct 仅初始化阶段，逐日不回写）。
  - 每笔 5% 概率发 `coupon`（face_value ∈ {10,20,50,100} 分→Money，min_spend ∈ {0,50,100}，campaign 随机，issue/expire=biz_date）。

### 6.4 risk 域生成

- **静态**（`WriteRiskStatic(ctx,db,cfg,custIDs)`，幂等）：
  - `risk_rule`：5 条（R001 单笔大额转账 / R002 频繁交易 / R003 异地登录 / R004 非工作时间大额 / R005 黑名单命中）；`condition_json` 文本。
  - `blacklist`：`n = max(2, int(20×sf))` 条；cust_id 随机；reason ∈ {欺诈,洗钱嫌疑,投诉涉诉}；effective=StartBizDate、expire=EndBizDate（rng `seed+32`）。
- **逐日**（`RunRisk(ctx,db,cfg,custIDs,accountNos)`）：`bizDateRange` 逐日，每日独立 rng（`seed+33+ordinal`），`pg.RunInTx` 内 `DELETE risk_event WHERE biz_date=$1` + 批量 INSERT：
  - `factor`；`n = max(0, int(5×sf×factor))`（周末由 cyclical×0.60 自然压低）。
  - 每条：rule_id 随机；action ∈ {拦截,放行,人工}；risk_score `uniform(0.3,0.95)`；cust_id/account_no 随机；txn_ref/summary 确定。

### 6.5 rng 偏移表（避碰撞）

已用：`+2`（core 余额）、`+10`（customer）、`+20`（payment）、`+100`/`+200`（core bizdate 量/内容）。B-4a 分配：reward `+30`（静态）/`+31`（逐日）、risk `+32`（静态）/`+33`（逐日）。预留 `+40`/`+41`（loan）、`+50`/`+51`（wealth）给 B-4b。确切偏移 plan 阶段在生成器内固化并加注释。

### 6.6 确定性关联

reward/risk 的 `cust_id` 从 customer 域生成的 `custIDs` 随机选（编号规则 `C%07d` 与 core/customer 一致）；risk 的 `account_no` 从 core `demandNos` 选。`seed/main.go` 显式传入 `custIDs`/`demandNos`（对齐 B-1 §6.2 显式编排风格）。

---

## 7. FDW 跨库联邦

### 7.1 Mappings 扩展

`internal/platform/fdw/fdw.go` 的 `Mappings` 末尾追加 2 条：

```go
{Host: "reward_db", Remote: "cust_db", Tables: []string{"cust_info"}},        // 原型已有
{Host: "risk_db",   Remote: "cust_db", Tables: []string{"cust_info"}},        // B-4a 新增
```

`SetupFDW` 逻辑不变（幂等 DROP→CREATE→IMPORT→RENAME），只是遍历更多 Mapping。host 仍 `localhost`（pg 进程连自己实例的其他库）。

### 7.2 联邦查询端点

- **reward** `GET /api/v1/reward/customers/{cust_id}/profile`：在 reward_db 执行
  `points_acct`(本地) JOIN `ext_cust_db_cust_info`(FDW) → 积分余额 + 会员等级 + 客户姓名/类型。
- **risk** `GET /api/v1/risk/events/{event_id}`：在 risk_db 执行
  `risk_event`(本地) JOIN `ext_cust_db_cust_info`(FDW) → 事件详情 + 客户姓名/等级。

两端点真正发起跨库 JOIN，验证「本域为主 + FDW 关联 cust_info」的联邦模式。

---

## 8. 服务纵切（reward / risk）

### 8.1 分层结构（对齐 B-1 §8.1）

```
internal/
├── reward/             # 新
│   ├── domain/         # PointsAcct/PointsTxn/Coupon/Campaign/MemberLevel + Money（int64 分）
│   ├── repo/           # pgx 落库查询 + 跨库 FDW JOIN
│   ├── service/        # 查询编排（纯逻辑，可单测）
│   └── api/            # http handlers + chi router
├── risk/               # 新
│   ├── domain/         # RiskRule/RiskEvent/Blacklist（无 Money）
│   ├── repo/
│   ├── service/
│   └── api/
└── (corebanking/customer/payment 不动)
```

`cmd/reward/main.go`、`cmd/risk/main.go`：依赖装配（pg 连自己的库 → repo → service → api → router → 起 server），参照 `cmd/customer/main.go`。

### 8.2 reward 金额（自带 Money）

reward domain 定义**自己的 `Money` 类型**（int64 分，禁 float，`String()` 输出 NUMERIC 文本，与 core/payment `Money` 同构），覆盖 `coupon.face_value`/`min_spend`/`coupon_usage.deduct_amount`，保持 reward domain 包**独立自包含**（离开 core-banking 也能编译）。repo 做分↔NUMERIC 转换，对齐 Spec A `txn_repo.go` 范式。

### 8.3 risk 无金额

risk domain 不引入 Money：`risk_score`/`threshold` 作 NUMERIC 文本直存（`string` 或 `float64`，repo 层透传），符合「金额才走 int64 分」的约束边界。

---

## 9. 只读 API（端点清单）

**reward 服务** (:8083 / reward_db)
- `GET /healthz`
- `GET /api/v1/reward/points-accounts/{cust_id}` — 积分账户详情（本库）
- `GET /api/v1/reward/points-accounts?member_level=&offset=&limit=` — 列表（本库）
- `GET /api/v1/reward/customers/{cust_id}/coupons?status=&offset=&limit=` — 客户优惠券（本库）
- `GET /api/v1/reward/customers/{cust_id}/profile` — 🔗 **联邦**：points_acct JOIN ext_cust_db_cust_info

**risk 服务** (:8084 / risk_db)
- `GET /healthz`
- `GET /api/v1/risk/events?from=&to=&rule_id=&action=&offset=&limit=` — 事件列表（本库）
- `GET /api/v1/risk/events/{event_id}` — 🔗 **联邦**：risk_event JOIN ext_cust_db_cust_info
- `GET /api/v1/risk/rules` — 规则列表（本库静态）
- `GET /api/v1/risk/blacklists?cust_id=&offset=&limit=` — 黑名单（本库）

写操作不在 B-4a（reward/risk 无总账）。

---

## 10. seed 编排扩展（6 → 8 步）

`cmd/seed/main.go` 的 `allDBs` 加 reward_db/risk_db；`runSeed` 在 payment（5/8）后、setup_fdw 前插入：

```
1/8 建 5 库（ensureDBs 循环 +2）
2/8 建 5 库表（各库 migrate.Run +2）
3/8 core（已有）
4/8 customer（已有，产出 custIDs）
5/8 payment（已有）
6/8 reward：WriteRewardStatic(ctx,rewardDB,cfg,custIDs) → RunReward(ctx,rewardDB,cfg,custIDs)
7/8 risk：  WriteRiskStatic(ctx,riskDB,cfg,custIDs) → RunRisk(ctx,riskDB,cfg,custIDs,demandNos)
8/8 setup_fdw（5 库联邦，Mappings +2）
```

`custIDs`（customer 步产出）、`demandNos`（core 步产出）显式传入 reward/risk。

---

## 11. 模板契约

### 11.1 template.yaml

`databases` 加 reward_db/risk_db；`services` 加 reward(8083)/risk(8084)；`version` → `0.3.0`。

```yaml
databases:
  - {name: core_db, migrate: db/migrations/core_db.sql}
  - {name: cust_db, migrate: db/migrations/cust_db.sql}
  - {name: pay_db, migrate: db/migrations/pay_db.sql}
  - {name: reward_db, migrate: db/migrations/reward_db.sql}   # 新
  - {name: risk_db, migrate: db/migrations/risk_db.sql}        # 新
services:
  - {name: core-banking, port: 8080, db: core_db}
  - {name: customer, port: 8081, db: cust_db}
  - {name: payment, port: 8082, db: pay_db}
  - {name: reward, port: 8083, db: reward_db}                  # 新
  - {name: risk, port: 8084, db: risk_db}                      # 新
```

### 11.2 docker-compose.yaml

加 reward/risk 两 service（`build: .`、`<<: *svcenv`、`DB_NAME`、`API_PORT`、端口映射、`depends_on: postgres.service_healthy`），参照 customer/payment 定义。

### 11.3 打包

改后须 `go generate ./internal/template` 重打包 `templates.tar`（Makefile test/e2e 已依赖 generate）。

---

## 12. 测试策略（三层 + FDW + e2e，对齐 B-1 §10 / B-2）

1. **fixture 确定性**：reward/risk 域，同 Seed+Scale → 同样的行，哈希比对；**周末日均量 < 工作日**（三因子 cyclical×0.60 生效）；季末/节假日 spike 抽样。
2. **service 层纯逻辑单测**：reward/risk 查询编排、联邦 JOIN 的 SQL 构造（service/repo 边界单测）。
3. **repo + api 集成**：真 pg，各服务连自己的库，断言本库查询 + 联邦 JOIN 查询返回客户姓名/等级。
4. **FDW 联邦集成测试**：`setup_fdw` 后，断言 2 个联邦端点 JOIN 到 cust_db 客户数据。
5. **e2e 冒烟**：Makefile/CI `jiade init → up（5 服务）→ seed → curl` 五服务 `/healthz` + 2 联邦端点。

---

## 13. 错误处理（对齐 Spec A §10 / B-1 §11）

- **FDW 查询失败**（外部表未建 / server 缺失）：透出 pg 原始错误，5xx；e2e/集成测试保证 `setup_fdw` 先于服务查询。
- **建库/建表失败**（pg 没起）：`ensureDBs` 短暂重试后透出「请先 make up」。
- **多库 partial 失败**：seed 编排中任一库失败即整体失败 + 非零退出，stderr 透传。
- **端口冲突**：compose 固定 8083/8084/...；冲突由 compose 自身报错，`.env.example` 写明改法。
- **`--reset`**：仍是所有破坏性操作的显式确认（DROP 5 库重建）。

---

## 14. 验收标准

1. jiade 仓自身：`go build ./...` 通过、`go test ./...` 全绿（不含 `templates/`）。
2. `templates/bank/` 独立 module：拷到临时目录后 `go build ./... && go test ./...` 全绿（CI 单独步骤）。
3. `jiade init --template bank` 产出含 reward/risk 服务（compose 5 Go 服务定义、template.yaml 5 库 5 服务）。
4. `cd /tmp/mybank && jiade up && jiade seed` 后：curl reward:8083 / risk:8084 的 `/healthz` 均 200。
5. 2 个联邦端点返回跨库数据：reward `/customers/{id}/profile` 有客户姓名；risk `/events/{id}` 有客户姓名/等级。
6. 同 Seed+Scale 两次 seed 产出完全一致；reward/risk 周末日均量 < 工作日（确定性 + 三因子单测）。
7. 生成物自包含：未安装 jiade（仅 docker+go）下也能 `docker compose up`（5 服务）+ `go run ./cmd/seed`。

---

## 15. 后续子 spec 预告

- **B-4b（loan + wealth）**：复制 B-1 模式 + 承接 B-2 逐日滚存——`loan_balance`（loan_no+biz_date）每日摊销快照、`wealth_nav`（product+biz_date）每日净值；loan 月度还款 + 逾期五级分类滑落；wealth 每日申赎量受三因子影响。rng 偏移预留 `+40`/`+41`（loan）、`+50`/`+51`（wealth）。

---

## 16. 必须延续的 Spec A / B-1 约束

- **自闭**：jiade 不联动 SCV/Porto，无 doctor。
- **生成物自包含**：bank 工程离开 jiade 可 `docker compose up` + `go run`。
- **金额 int64 分，禁 float**（reward 域 Money + repo 分↔NUMERIC；risk 无金额）。
- **复式记账只在 core**：reward/risk 无总账。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；各域 domain 互不依赖。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改后须 `go generate ./internal/template` 重打包 `templates.tar`。
- **go 1.22**：bank module 的 `go.mod` pin `1.22`；本地验证 macOS 15 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。

---

## 17. 开放问题

- 无待决问题。B-4a 的范围（reward+risk，B-4b 留 loan+wealth）、fixture 深度（逐日三因子忠实移植）、risk 联邦（新增 risk_db←cust_info）、reward Money（自带 int64 分）、API 焦点（每服务 1 联邦端点）均已确认。
