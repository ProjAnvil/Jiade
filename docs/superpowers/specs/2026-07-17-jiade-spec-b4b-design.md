# Jiade — Spec B-4b 设计文档

- **日期**：2026-07-17
- **状态**：设计待审（待用户复审）
- **范围**：Spec B-4b —— `loan` + `wealth` 两服务纵切（schema / 确定性 fixture / 四层只读 API / FDW 跨库联邦），B-4 的「逐日滚存」半
- **作者**：yuhaochen × Claude（brainstorming）
- **上游**：B-1（多库 + FDW + 四层模式）、B-2（distribution 三因子 + 逐日滚存内核）、B-4a（reward/risk 纵切 + 三因子横向复用）

---

## 1. 背景与定位

### 1.1 B-4 收尾

B-4 按 B-4a 再分为两半：B-4a（reward+risk，三因子事件流、无逐日快照）已合并 main（HEAD `1b65e62`）。B-4b 是 B-4 的另一半，也是 Spec B 最后一块——`loan` + `wealth` 两服务，**逐日滚存**形态（每日余额/净值快照 + 状态内存滚存），比 B-4a 的事件流更重。

### 1.2 B-4b 的位置

把 B-1 的「每加一个服务 = 加一个库 + 四层只读 + FDW 联邦」模式**再复制两条纵切**（loan、wealth），并首次落地两类新形态：

- **loan**：`loan_balance` 每日每借据全量快照（形态同 core `account_balance`，路径依赖滚存）+ 月度 `loan_repay` + 逾期 `loan_overdue` 五级分类滑落。**确定性摊销，不走三因子**。
- **wealth**：混合形态——`wealth_nav` 每日每产品净值游走（路径依赖全量快照）+ `wealth_order` 每日三因子事件流（同 reward/risk）+ `wealth_income` 每日确定性利息。

### 1.3 与既有 spec 的关系

**填内容，不改架构**。B-1 多库基建、B-2 三因子+滚存内核、B-4a reward/risk 四层模板与 FDW/seed 扩展点全部沿用。core/customer/payment/reward/risk/bizdate.go 代码**不重构、不改动**。

---

## 2. 目标与非目标

### 2.1 B-4b 目标

1. **loan 服务**（loan_db）：贷款域 6 表 schema（忠实还原）+ 确定性 fixture（静态一次性 + 月度还款 + 逐日快照滚存 + 逾期滑落）+ 完整四层只读 API + 1 联邦端点。
2. **wealth 服务**（wealth_db）：理财域 5 表 schema + 确定性 fixture（静态 + 每日 NAV 游走 + 每日三因子订单 + 每日利息）+ 完整四层只读 API + 1 联邦端点。
3. **多库扩展**：postgres 实例从 5 库扩到 7 库（+loan_db/wealth_db）；compose/template.yaml 加 2 服务 2 端口。
4. **FDW 联邦扩展**：`Mappings` 加 loan_db/wealth_db ← cust_db.cust_info；两服务各 1 跨库 JOIN 端点。
5. **滚存形态落地**：loan_balance/wealth_nav 逐日全量快照（路径依赖、需全量重放、形态同 core account_balance），验证 B-2 滚存范式可迁移到 core 之外。
6. **测试与验收**：确定性（含 loan 逾期滑落、wealth 周末订单 < 工作日）、各层单测、repo+api 集成、FDW 联邦集成、seed_test 全量 seed。

### 2.2 B-4b 非目标

- **写操作 HTTP 接口** → core 已由 B-3 覆盖；loan/wealth 无总账，不做写接口。
- **loan/wealth 与 core 账务的真实一致性** → 缩影合理简化，各域独立生成（对齐 B-1 §6.5）。
- **原型未生成的字段补全** → 除 `wealth_income`（本次按 Q1-B 补每日利息）外，其余保持原生成范围。
- **`meta` 指标层** → 不在 Jiade 范围。
- **重构既有代码** → core/customer/payment/reward/risk/bizdate.go 不动（仅在 bizdate.go 加一个 `addMonths` helper）。

---

## 3. 核心原则（延续 Spec A / B-1 / B-4a）

| 原则 | 在 B-4b 的体现 |
|------|---------------|
| **生成物自包含** | 拷出的工程离开 jiade 也能 `docker compose up`（7 服务）+ `go run ./cmd/seed`。 |
| **自闭** | jiade 不联动 SCV/Porto；B-4b 只填充 bank 模板内容。 |
| **缩影哲学** | 保留模式（多库 + FDW + 微服务 + 分层 + 逐日滚存/三因子），砍规模（dev 级行数）。 |
| **金额 int64 分，禁 float** | loan 金额字段全 int64 分（自带 Money）；wealth 金额字段 int64 分，NAV/share/expected_return 是非货币小数按 NUMERIC 文本直存。 |
| **复式记账只在 core** | loan/wealth 无总账；service 层只做查询编排。 |
| **依赖方向向内** | 各服务 `api → service → repo → domain`；各域 domain 互不依赖。 |
| **确定性** | 同 Seed+Scale → 同样的行；wealth 订单每日独立 rng 单日可复现；loan 滚存全量重放确定；跨域编号规则一致关联。 |

---

## 4. 架构总览

### 4.1 服务拓扑（7 进程 + 7 库，单 postgres 实例）

| 服务 | 端口 | 库 | cmd 入口 | 状态 |
|------|------|----|---------|------|
| core-banking | 8080 | core_db | `cmd/core-banking/main.go` | 已有 |
| customer | 8081 | cust_db | `cmd/customer/main.go` | 已有 |
| payment | 8082 | pay_db | `cmd/payment/main.go` | 已有 |
| reward | 8083 | reward_db | `cmd/reward/main.go` | 已有（B-4a） |
| risk | 8084 | risk_db | `cmd/risk/main.go` | 已有（B-4a） |
| **loan** | **8085** | **loan_db** | `cmd/loan/main.go` | **新** |
| **wealth** | **8086** | **wealth_db** | `cmd/wealth/main.go` | **新** |

每域一个独立 Go 进程，compose 里各一个 service 定义 + 容器 + 端口，沿用 B-1 的 `&svcenv` 锚点 + `restart: unless-stopped` + `depends_on: postgres.service_healthy`。

### 4.2 数据库拓扑

```
postgres 实例（容器 bank-postgres）
├── core_db    ← core-banking（已有）
├── cust_db    ← customer（已有）
├── pay_db     ← payment（已有）
├── reward_db  ← reward（已有）
├── risk_db    ← risk（已有）
├── loan_db    ← loan（新）
└── wealth_db  ← wealth（新）

FDW 外部表（seed 末尾 setup_fdw 建立，命名 ext_{remote}_{tbl}）：
├── loan_db:   ext_cust_db_cust_info          ← B-4b 新增
└── wealth_db: ext_cust_db_cust_info          ← B-4b 新增
```

### 4.3 数据流

```
jiade init --template bank --dir ./mybank
   └─ copy templates/bank/. → ./mybank/（含 loan/wealth 两服务源码）
jiade up   └─ docker compose up -d（postgres + 7 服务）
jiade seed └─ go run ./cmd/seed --scale=dev --reset
              （建 7 库 → 建 7 库表 → core → customer → payment → reward → risk → loan → wealth → setup_fdw）
curl localhost:8085/api/v1/loan/accounts/{loan_no}/profile      # 跨库 FDW JOIN
curl localhost:8086/api/v1/wealth/holdings/{holding_id}/profile # 跨库 FDW JOIN
```

---

## 5. schema（忠实还原 + 补索引）

### 5.1 loan_db（6 表）

`loan_product` / `loan_account` / `loan_disbursement` / `loan_repay` / `loan_overdue` / `loan_balance`。DDL 列定义固定（零差异），仅**补索引**（`biz_date` / `loan_no` / `cust_id` / `product_code`），对齐 reward/risk DDL 的索引风格。金额字段（`principal`/`balance`/`amount`/`principal_amt`/`interest_amt`/`paid_*`/`overdue_amount`/`principal_balance`/`interest_receivable`/`max_amount`）为 `NUMERIC(18,2)`；`rate`/`min_rate`/`max_rate` 为 `NUMERIC(10,6)`（比率，非金额）。

### 5.2 wealth_db（5 表）

`wealth_product` / `wealth_nav` / `wealth_holding` / `wealth_order` / `wealth_income`。DDL 列定义固定，补索引。金额字段（`min_amount`/`cost`/`current_value`/`amount`）为 `NUMERIC(18,2)`；`nav`/`accum_nav` 为 `NUMERIC(12,6)`、`share` 为 `NUMERIC(18,4)`、`expected_return` 为 `NUMERIC(10,6)`（均非货币小数，按 NUMERIC 文本直存）。

### 5.3 数据策略（建表全、数据聚焦）

| 表 | 数据 | 端点 |
|----|------|------|
| loan_product | 静态全量（4 产品） | 产品列表 |
| loan_account | 静态（`len(custIDs)/4` 借据） | 详情 / 列表 / 联邦 |
| loan_disbursement | 静态（每借据 1 放款） | — |
| loan_repay | **月度**（月初生成当月计划） | （可查，非核心端点） |
| loan_overdue | **逐日**（逾期借据，五级分类滑落） | 逾期列表 |
| loan_balance | **逐日全量快照**（每借据每日） | 余额列表（日期范围） |
| wealth_product | 静态全量（6 产品） | 产品列表 |
| wealth_nav | **逐日全量游走**（每产品每日净值） | NAV 序列 |
| wealth_holding | 静态（每客户 0-3 个） | 持仓列表 / 联邦 |
| wealth_order | **逐日三因子**（量=20×sf×factor） | 订单列表 |
| wealth_income | **逐日**（每持仓 cost×ret/365 利息，B-4b 补） | 收入列表 |

---

## 6. fixture 生成（滚存 + 三因子混合，复用 B-2/B-4a 内核）

### 6.1 文件落点与复用

新增 `templates/bank/internal/fixtures/domains/loan.go`、`wealth.go`，**与 `bizdate.go`/`reward.go`/`risk.go` 同 `domains` 包**，直接调用未导出的 `trendFactor`/`seasonalFactor`/`cyclicalFactor`/`bizDateRange`/`dayOrdinal`/`dateCompact`/`placeholders`/`nullable`/`parseDate`/`parseDate2`，以及 `reward.go` 的 `maxInt`/`minInt`/`addDays`。批量写入复用 `placeholders(nRows,nCols)` + `pg.RunInTx` 范式。`fixtures.ScaleFactor`（B-4a 已加）直接用。

### 6.2 bizdate.go 新增 helper

```go
// addMonths 把 YYYY-MM-DD 加 n 月（loan mature_date = start + term 用）。
func addMonths(dateStr string, n int) string {
    t, err := parseDate2(dateStr)
    if err != nil { return dateStr }
    return t.AddDate(0, n, 0).Format("2006-01-02")
}
```

### 6.3 loan 域生成（rng +40 静态 / +41 滚存；无三因子）

**词库（rng.go 新增）**：`LoanProducts`（4 产品元组：code/name/loan_type/cust_type/min_rate/max_rate/max_term/max_amount）、`GuaranteeTypes=["信用","抵押","保证"]`、`OverdueClasses`（5 档阈值表 `[(0,正常),(1,关注),(30,次级),(90,可疑),(180,损失)]`）；复用 `Branches`。

- **GenLoanStatic(cfg, custIDs) → {Products, Accounts, Disbursements}**（rng `seed+40`）：
  - products：固定 4 行。
  - `nLoans = maxInt(5, len(custIDs)/4)`（派生，零 Counts 改动）。
  - 每借据 `i`：cust/product 随机选；principal = `clamp(IntRange(0,99999) × (maxAmtYuan/100000), 10000元, maxAmt)`（纯整数 → cents）；rate = `min + Float64()×(max-min)` 格式 6dp（比率，非金额，float 可接受）；term = max_term≥36 ? Choice[12,24,36] : Choice[12,24]；start = `RandomDate(start, max(start, addMonths(end,-1)))`（短区间守卫）；mature=`addMonths(start,term)`；loan_no=`LN%07d`；disb：`LN-DB-%07d`/amount=principal/to_account=`D%010d`。**全程确定性 ID，无 uuid**。
- **WriteLoanStatic**：幂等 DELETE loan_disbursement→loan_account→loan_product；INSERT 三者（money 走 `.String()`）。
- **RunLoan(ctx, db, cfg, accounts)**（rng `seed+41`，单次——无逐日随机）：
  - state[loanNo] = `{balance Money, overdueDays int, overdueStart string, monthlyPrincipal Money, rateFloat float64}`；`monthlyPrincipal = NewMoneyFromCents(roundDiv(principal.Cents(), termMonths))`（四舍五入到分）。
  - 逾期选择：遍历 accounts，`if IntRange(1,12)==1 → overdueStart=RandomDate(start, max(start, addMonths(end,-2)))`（~8%）。
  - 逐日（`bizDateRange`），**每日一个 `pg.RunInTx`**：
    - 月初（`d.Month()!=lastMonth`）：对 balance>0 借据造 repay 行——principalAmt=`min(monthlyPrincipal,balance)`；interestAmt=`NewMoneyFromCents(round(balance.Cents()×rateFloat/12))`；若 `dateStr>=overdueStart`→status=overdue 不扣；否则 balance=`balance.Sub(principalAmt)`（clamp≥0）、paid=全额。repay_id=`LN-RP-{compact}-{loanIdx}`。`DELETE loan_repay WHERE biz_date=$1` + bulkInsert。
    - 累计逾期天数：`dateStr>overdueStart`（ISO 日期字典序可比较）→ `overdueDays=(d-overdueStart).days`。
    - 当日快照：balance 行（balance>0：principalBalance=balance，interestReceivable=`round(balance.Cents()×rateFloat/360)` 分）；overdue 行（overdueDays>0 且 dateStr>overdueStart：class=`overdueClass(days)`，amount=balance）。`DELETE loan_balance/loan_overdue WHERE biz_date=$1` + bulkInsert ×2。
  - bulkInsert ×3（repay 9列 / balance 4列 / overdue 6列），`placeholders`+`bizDateBatchSize`。

### 6.4 wealth 域生成（rng +50 静态 / +51 逐日；混合三因子）

**词库（rng.go 新增）**：`WealthProducts`（6 产品元组：code/name/type/risk/expected_return/min_amount/term_days）、`OrderTypes=["申购","申购","赎回"]`（2/3 申购）、`IncomeTypes=["利息"]`。

- **GenWealthStatic(cfg, custIDs, demandAccounts) → {Products, Holdings}**（rng `seed+50`）：
  - products：固定 6 行（start=cfg.StartBizDate, end=`addDays(cfg.EndBizDate,365)`）。
  - 每客户 `IntRange(0,3)` 个持仓：prod 随机；nav0=`1+Float64()×0.25`（4dp）；amount_yuan=`max(minAmtYuan, IntRange(0,99999)×100)`→cents；share=`amount/nav0`（4dp string，非金额）；holding_id=`WP-HD-%07d`；cost=current_value=amount；account_no 随机 demandAccount。
- **WriteWealthStatic**：幂等 DELETE wealth_holding→wealth_product；INSERT。
- **RunWealth(ctx, db, cfg, products, holdings, custIDs, demandAccounts)**：
  - sf=ScaleFactor；navState[prod]=`1+ret/365`（ret=解析 expected_return，比率 float）；holdingCost map（holdingID→{costCents, prodCode}）供 income 用。
  - 逐日，**每日一个 tx**，rng=`seed+51+dayOrdinal`（per-day，对齐 reward）：
    - nav 行（每产品）：drift=`navState×(1+(Float64()×0.006-0.002))`；navState=`max(0.5,drift)`（6dp）；accumNav=`navState×1.1`（6dp）。
    - orders：`n=maxInt(0,int(20×sf×factor))`（**三因子**，factor=trend×seasonal×cyclical）；每笔 cid/prod 随机、type=Choice(OrderTypes)、amount 同持仓公式、share=4dp、nav=navState[prod]、id=`WP-OD-{compact}-{i}`。
    - incomes（Q1-B）：每持仓 `amount=round(holdingCost.Cents()×productRet/365)` 分、type="利息"、id=`WP-IC-{compact}-{holdingIdx}`。
    - `DELETE wealth_nav/order/income WHERE biz_date=$1` + bulkInsert ×3（nav 4列 / order 10列 / income 5列）。

### 6.5 Jiade 适配（相对原型的有意偏离，均符合既有约定）

| 点 | 原型 | Jiade | 理由 |
|---|---|---|---|
| 逐日随机 rng | 单 faker 贯穿全循环 | per-day `seed+off+ordinal`（loan 例外：无逐日随机，单次 seed+41） | 对齐 reward/risk/core 既有约定；全量重放仍确定 |
| ID | uuid4 | 确定性前缀+序号 | 对齐全部既有域（可复现/可哈希比对） |
| balance 滚存 | float yuan 每步 round | Money int64 分 `Sub` | 金额禁浮点（核心约束） |
| interest/nav 计算 | float | 比率×Money-cents→round 到分 / NAV 6dp float 游走 | rate/nav 是比率非金额，中间 float 可接受（结果落 Money/6dp），与原型一致 |
| wealth_income | 不生成 | 每日利息（Q1-B） | brainstorming 选 B，让 incomes 有数据可查 |
| wealth 持仓 rng | 与 daily 共享 faker+51 | 归入静态批次 +50（与 products 共享） | 避免与 daily +51+ordinal 碰撞，保持「静态 N / 逐日 N+1+ordinal」reward 风格 |

### 6.6 rng 偏移表（避碰撞，B-4a 预留确认）

已用：`+2`（core 余额）、`+10`（customer）、`+20`（payment）、`+30`/`+31`（reward）、`+32`/`+33`（risk）、`+100`/`+200`（core bizdate）。B-4b 分配：loan `+40`（静态）/`+41`（滚存，单次）、wealth `+50`（静态，含持仓）/`+51`（逐日，+ordinal）。全部偏移在生成器内固化并加注释。

### 6.7 确定性关联

loan 的 `cust_id` 从 customer 域 `custIDs` 随机选；wealth 的 `cust_id` 同，`account_no` 从 core `demandNos` 选。`seed/main.go` 显式传入 `custIDs`/`demandNos`（对齐 B-1 §6.2 显式编排风格）。编号规则 `C%07d`（客户）/活期账户号与 core/customer 一致。

### 6.8 滚存与事件流的形态对照

| 形态 | 代表表 | 路径依赖 | 重放要求 | 对应既有 |
|------|--------|---------|---------|---------|
| 逐日全量快照（滚存） | loan_balance / wealth_nav | 是（balance/nav 跨日滚） | 全量重放才正确 | core account_balance |
| 逐日事件流（三因子） | wealth_order | 否（每日独立 rng） | 单日可复现 | reward points_txn / risk risk_event |
| 月度/逾期衍生物 | loan_repay / loan_overdue | 是（依附 loan 滚存） | 随 loan 全量重放 | — |

---

## 7. FDW 跨库联邦

### 7.1 Mappings 扩展

`internal/platform/fdw/fdw.go` 的 `Mappings` 末尾追加 2 条：

```go
{Host: "loan_db",   Remote: "cust_db", Tables: []string{"cust_info"}},   // 原型已有
{Host: "wealth_db", Remote: "cust_db", Tables: []string{"cust_info"}},   // 原型已有
```

`SetupFDW` 逻辑不变（幂等 DROP→CREATE→IMPORT→RENAME），只是遍历更多 Mapping。

### 7.2 联邦查询端点

- **loan** `GET /api/v1/loan/accounts/{loan_no}/profile`：在 loan_db 执行 `loan_account`(本地) JOIN `ext_cust_db_cust_info`(FDW) → 借据本金/余额 + 客户姓名/类型。
- **wealth** `GET /api/v1/wealth/holdings/{holding_id}/profile`：在 wealth_db 执行 `wealth_holding`(本地) JOIN `ext_cust_db_cust_info`(FDW) → 持仓份额/市值 + 客户姓名/类型。

两端点真正发起跨库 JOIN，验证「本域为主 + FDW 关联 cust_info」的联邦模式。

---

## 8. 服务纵切（loan / wealth）

### 8.1 分层结构（对齐 B-1 §8.1 / B-4a §8.1）

```
internal/
├── loan/               # 新
│   ├── domain/         # LoanProduct/LoanAccount/LoanDisbursement/LoanRepay/LoanOverdue/LoanBalance + LoanProfile + Money（int64 分）
│   ├── repo/           # pgx 落库查询 + 跨库 FDW JOIN
│   ├── service/        # 查询编排（薄封装）
│   └── api/            # http handlers + chi router
├── wealth/             # 新
│   ├── domain/         # WealthProduct/WealthNav/WealthHolding/WealthOrder/WealthIncome + WealthProfile + Money（int64 分）
│   ├── repo/
│   ├── service/
│   └── api/
└── (corebanking/customer/payment/reward/risk 不动)
```

`cmd/loan/main.go`、`cmd/wealth/main.go`：依赖装配（pg 连自己的库 → repo → service → api → router → 起 server），参照 `cmd/reward/main.go`（env `DB_NAME`/`API_PORT`、ping 重试、graceful shutdown）。

### 8.2 loan 金额（自带 Money）

loan domain 定义**自己的 `Money` 类型**（从 reward domain 逐字复制：int64 分，禁 float，`String()` 输出 NUMERIC(18,2) 文本），覆盖全部金额字段；`rate`/`min_rate`/`max_rate` 作 NUMERIC(10,6) 文本直存（`string`）。repo 做分↔NUMERIC 转换。

### 8.3 wealth 金额（自带 Money + 非货币小数文本）

wealth domain 同样复制 `Money`，覆盖 `amount`/`cost`/`current_value`/`min_amount`；`nav`/`accum_nav`/`share`/`expected_return` 是非货币小数，按 NUMERIC 文本直存（`string`），对齐 risk 的 risk_score 处理边界。

---

## 9. 只读 API（端点清单）

**loan 服务** (:8085 / loan_db)
- `GET /healthz`
- `GET /api/v1/loan/products` — 产品列表（静态）
- `GET /api/v1/loan/accounts?product_code=&status=&offset=&limit=` — 借据列表
- `GET /api/v1/loan/accounts/{loan_no}` — 借据详情（404 if missing）
- `GET /api/v1/loan/balances?from=&to=&loan_no=&offset=&limit=` — 逐日余额快照（日期范围）
- `GET /api/v1/loan/overdue?overdue_class=&from=&to=&offset=&limit=` — 逾期五级分类
- `GET /api/v1/loan/accounts/{loan_no}/profile` — 🔗 **联邦**：loan_account JOIN ext_cust_db_cust_info

**wealth 服务** (:8086 / wealth_db)
- `GET /healthz`
- `GET /api/v1/wealth/products` — 产品列表（静态）
- `GET /api/v1/wealth/nav?product_code=&from=&to=` — 每日净值序列
- `GET /api/v1/wealth/holdings?cust_id=&offset=&limit=` — 持仓列表
- `GET /api/v1/wealth/orders?cust_id=&product_code=&from=&to=&offset=&limit=` — 订单列表（三因子）
- `GET /api/v1/wealth/incomes?holding_id=&from=&to=&offset=&limit=` — 收入列表
- `GET /api/v1/wealth/holdings/{holding_id}/profile` — 🔗 **联邦**：wealth_holding JOIN ext_cust_db_cust_info

日期过滤一律 `NULLIF($1,'')::date`（B-4a 教训：`date>=text` 会 PREPARE 失败）；分页 `limit<=0`→50。写操作不在 B-4b（loan/wealth 无总账）。

---

## 10. seed 编排扩展（8 → 10 步）

`cmd/seed/main.go` 的 `allDBs` 加 loan_db/wealth_db；`runSeed` 在 risk（7/10）后、setup_fdw 前插入 loan/wealth 两步：

```
1/10 建 7 库（ensureDBs 循环 +2）
2/10 建 7 库表（各库 migrate.Run +2）
3/10 core（已有）
4/10 customer（已有，产出 custIDs）
5/10 payment（已有）
6/10 reward（已有）
7/10 risk（已有）
8/10 loan：  GenLoanStatic(cfg,custIDs) → WriteLoanStatic(loanDB,...) → RunLoan(loanDB,cfg,accounts)
9/10 wealth：GenWealthStatic(cfg,custIDs,demandNos) → WriteWealthStatic(wealthDB,...) → RunWealth(wealthDB,cfg,products,holdings,custIDs,demandNos)
10/10 setup_fdw（7 库联邦，Mappings +2）
```

`custIDs`（customer 步产出）、`demandNos`（core 步产出）显式传入 loan/wealth。日志字符串改 `N/10`。

---

## 11. 模板契约

### 11.1 template.yaml

`databases` 加 loan_db/wealth_db；`services` 加 loan(8085)/wealth(8086)；`version` → `0.4.0`；description 加「+loan+wealth，Spec B-4b」。

```yaml
databases:
  - {name: core_db, migrate: db/migrations/core_db.sql}
  - {name: cust_db, migrate: db/migrations/cust_db.sql}
  - {name: pay_db, migrate: db/migrations/pay_db.sql}
  - {name: reward_db, migrate: db/migrations/reward_db.sql}
  - {name: risk_db, migrate: db/migrations/risk_db.sql}
  - {name: loan_db, migrate: db/migrations/loan_db.sql}      # 新
  - {name: wealth_db, migrate: db/migrations/wealth_db.sql}  # 新
services:
  - {name: core-banking, port: 8080, db: core_db}
  - {name: customer, port: 8081, db: cust_db}
  - {name: payment, port: 8082, db: pay_db}
  - {name: reward, port: 8083, db: reward_db}
  - {name: risk, port: 8084, db: risk_db}
  - {name: loan, port: 8085, db: loan_db}                    # 新
  - {name: wealth, port: 8086, db: wealth_db}                # 新
```

### 11.2 docker-compose.yaml

加 loan/wealth 两 service（build args `CMD: loan`/`wealth`、`<<: *svcenv`、`DB_NAME`、`API_PORT: 8085/8086`、端口映射、`depends_on: postgres.service_healthy`），参照 reward/risk 定义。

### 11.3 manifest_test.go

断言 5→7：services `+loan:8085/wealth:8086`、databases `+loan_db/wealth_db`；注释改「Spec B-4b」。

### 11.4 打包

改后须 `go generate ./internal/template` 重打包 `templates.tar`（整目录 tar，不识别 .gitignore；重打包前清 `templates/bank/` 下任何 go build 残留二进制——删除需先确认用户）。

---

## 12. 测试策略（对齐 B-4a）

1. **domain**：loan_test/wealth_test（struct 构造 + money parse round-trip）、money_test（副本）。
2. **api handlers**：loan/wealth handlers_test（httptest + fake service，404/分页/DTO 字段）。
3. **生成器单测**：loan_test/wealth_test（确定性——量级、ID 格式、money 不出 float、loan 逾期滑落档位正确、wealth 周末订单 < 工作日、哈希稳定）。
4. **seed_test（集成，真 pg 5433）**：扩断言——loan_balance 某 biz_date 有行、wealth_nav 每产品每日有行、loan_overdue 五级分类可查、wealth_order 周末 < 工作日、wealth_income 每持仓每日有行、FDW loan/wealth←cust_info 可 JOIN。沿用 B-4a 的 `TestMain` chdir 到模块根。

service 层薄封装（查询透传），无独立单测（对齐 reward/risk，由 handlers_test 用 fake store 覆盖）。

---

## 13. 错误处理（对齐 Spec A §10 / B-1 §11 / B-4a §13）

- **FDW 查询失败**（外部表未建 / server 缺失）：透出 pg 原始错误，5xx；seed 编排保证 `setup_fdw` 先于服务查询。
- **建库/建表失败**（pg 没起）：`ensureDBs` 短暂重试后透出「请先 make up」。
- **多库 partial 失败**：seed 编排中任一库失败即整体失败 + 非零退出，stderr 透传。
- **repo 日期过滤**：`NULLIF($1,'')::date`（避免 `date>=text` PREPARE 类型错误）。
- **`--reset`**：仍是所有破坏性操作的显式确认（DROP 7 库重建）。
- **删除文件**：删任何文件（含 build 残留二进制）需先确认用户。

---

## 14. 验收标准

1. jiade 仓自身：`go build ./...` 通过、`go test ./...` 全绿（不含 `templates/`）。
2. `templates/bank/` 独立 module：`go build ./... && go test ./...` 全绿（unit + integration）。
3. `jiade init --template bank` 产出含 loan/wealth 服务（compose 7 Go 服务定义、template.yaml 7 库 7 服务、version 0.4.0）。
4. `cd /tmp/mybank && jiade up && jiade seed` 后：curl loan:8085 / wealth:8086 的 `/healthz` 均 200。
5. 2 个联邦端点返回跨库数据：loan `/accounts/{loan_no}/profile` 有客户姓名；wealth `/holdings/{holding_id}/profile` 有客户姓名/类型。
6. 同 Seed+Scale 两次 seed 产出完全一致；loan 逾期借据五级分类随天数滑落；wealth 周末订单日均 < 工作日（确定性 + 滚存/三因子单测）。
7. 生成物自包含：未安装 jiade（仅 docker+go）下也能 `docker compose up`（7 服务）+ `go run ./cmd/seed`。
8. 末项 `go generate ./internal/template` 重打包 `templates.tar` 成功、manifest_test 7/7 通过。

---

## 15. 必须延续的 Spec A / B-1 / B-4a 约束

- **自闭**：jiade 不联动 SCV/Porto，无 doctor。
- **生成物自包含**：bank 工程离开 jiade 可 `docker compose up` + `go run`。
- **金额 int64 分，禁 float**（loan/wealth 金额 Money + repo 分↔NUMERIC；rate/nav/share/expected_return 非货币，文本直存；interest/nav 计算的中间 float 是比率运算、结果 round 落 Money/6dp，可接受）。
- **复式记账只在 core**：loan/wealth 无总账。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；各域 domain 互不依赖。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改后须 `go generate ./internal/template` 重打包 `templates.tar`。
- **go 1.22**：bank module 的 `go.mod` pin `1.22`；本地验证 macOS 15 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。
- **端口占用**：5432 可能被本地既有 postgres 容器占用（别碰，reset 会毁其数据）；集成测试用临时 pg 5433。

---

## 16. 开放问题

- 无待决问题。B-4b 的范围（loan+wealth）、形态切分（loan 全量摊销 / wealth NAV 游走 + 三因子订单）、wealth_income（Q1-B 每日利息）、量级派生（Q2-A 从 custIDs 派生，零 Counts 改动）、wealth 单循环结构（方案 1）、rng 偏移（+40/+41/+50/+51）、Money 切分、API 焦点（每服务 1 联邦端点）均已 brainstorming 确认。
