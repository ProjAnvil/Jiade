# Jiade Spec B-4b Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 bank 模板里纵切 loan + wealth 两个只读服务（loan_db 6 表 / wealth_db 5 表，DDL 忠实还原），落地「逐日滚存」形态（loan_balance/wealth_nav 全量快照，路径依赖）+ wealth 三因子订单事件流，每服务 1 个跨库 FDW JOIN 端点，使 `jiade init → up → seed` 后可 curl 七服务 healthz + 两个新联邦端点。

**Architecture:** 单 postgres 实例 7 库（core/cust/pay/reward/risk/loan/wealth），每域一个独立 Go 进程（`cmd/<域>` + `internal/<域>/{domain,service,repo,api}` 四层，逐字镜像 reward/risk）。loan：静态一次性生成（rng `seed+40`）+ 单次滚存循环（rng `seed+41`，无逐日随机；月初还款计划 + 逾期五级分类滑落 + 每日全量余额快照）。wealth：静态（rng `seed+50`，产品+持仓）+ 逐日循环（rng `seed+51+ordinal`；NAV 游走滚存 + 三因子订单 + 每日利息）。FDW `Mappings` 加 loan_db/wealth_db ← cust_db.cust_info。

**Tech Stack:** Go 1.22 · database/sql · math/rand/v2 PCG · net/http + chi v5 · postgres_fdw · postgres（集成测试 `//go:build integration`，本地用临时 pg 5433）。

## Global Constraints

> 每个任务的隐含前置条件。逐字来自 spec §2/§3/§13/§15 + B-1/B-2/B-4a 既有约束。

- **分支**：`spec/b4b-loan-wealth`（已检出，干净）。所有提交落此分支。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改完后在 **jiade 根** 跑 `go generate ./internal/template` 重打包 `templates.tar`（Task 12）。
- **go 1.22**：bank module `go.mod` pin 1.22；本地验证 macOS 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0` 前缀（Darwin 25 + go1.22 dyld bug）。CI（Linux）直接 `go test`。
- **bank 命令在 `templates/bank/` 下跑**；jiade 命令在仓根跑。
- **金额 int64 分，禁 float**：loan/wealth 金额走各自 domain 的 `Money`（从 `templates/bank/internal/reward/domain/money.go` 逐字复制），repo 分↔NUMERIC 转换。`rate`/`min_rate`/`max_rate`/`nav`/`accum_nav`/`share`/`expected_return` 是**非货币小数**，按 NUMERIC 文本直存（domain 字段 `string`）。interest/nav 计算的中间 float 是比率运算、结果 round 落 Money/6dp 文本，可接受（spec §6.5）。
- **复式记账只在 core**：loan/wealth 无总账，无写接口。
- **依赖方向向内** `api → service → repo → domain`；各域 `domain` 互不依赖；`repo` 不 import `service`。`fixtures/domains` 可 import 各 `domain`。
- **确定性**：同 Seed+Scale → 同样的行。确定 ID（`LN/LN-DB/LN-RP/LN-OD/WP-HD/WP-OD/WP-IC` 前缀+序号/日期，**非** uuid4——原实现用 uuid4 非确定，这里改确定 ID 是有意偏离）。rng 偏移：loan `+40`(静态)/`+41`(滚存，单次)，wealth `+50`(静态)/`+51`(逐日 +ordinal)。已占用偏移：+2/+30/+31/+32/+33/+100/+200，不得复用。
- **DATE/NUMERIC 扫描**：pgx stdlib 下 DATE 列可直接 scan 进 `string`（risk repo 已验证，无需 `::text`）；NUMERIC 金额 scan 进 `sql.NullString` 再 `domain.ParseCents`；NUMERIC 非金额 scan 进 `sql.NullString` 取 `.String`。
- **日期过滤 SQL**：一律 `NULLIF($1,'')::date`（`date>=text` 会 PREPARE 失败）；分页 `limit<=0`→50。
- **FDW server host 统一 `localhost`**，port 5432，user/pass bank/bank；外部表命名 `ext_{remote}_{tbl}`。
- **Dockerfile 已参数化** `ARG CMD` → `go build ./cmd/${CMD}`：加 loan/wealth 只需建 `cmd/loan`、`cmd/wealth` 目录 + compose `args: CMD`，**不改 Dockerfile**。
- **本地 5432 可能被本地既有 postgres 容器占用**：单元测试不需要 pg；集成/e2e 用临时 `docker run -p 5433:5432` + `DB_PORT=5433`（Task 12 给出确切命令）。**别碰既有容器的数据**（只许 stop/start 容器，且仅 e2e 需要）；seed `--reset` 只 DROP bank 的 7 个库。
- **不杀用户进程**：808x 端口被占（如 corpit）时记录并报告，不得 kill；受影响验证跳过并在报告里注明。
- **删除需确认**：本计划不删除任何既有文件；重打包前若发现 `templates/bank/` 下有 go build 残留二进制，**停下来报告**，不得自行删除。
- **core/customer/payment/reward/risk 代码不动**；bizdate.go 仅追加 `addMonths`。

## File Structure

**Create:**
- `templates/bank/db/migrations/loan_db.sql` — 6 表（loan_product/loan_account/loan_disbursement/loan_repay/loan_overdue/loan_balance）+ 补索引
- `templates/bank/db/migrations/wealth_db.sql` — 5 表（wealth_product/wealth_nav/wealth_holding/wealth_order/wealth_income）+ 补索引
- `templates/bank/internal/loan/domain/{money.go,loan.go}` + `money_test.go` + `loan_test.go`
- `templates/bank/internal/loan/repo/loan_repo.go` + `loan_repo_test.go`（integration）
- `templates/bank/internal/loan/service/loan_service.go`
- `templates/bank/internal/loan/api/{handlers.go,router.go}` + `handlers_test.go`
- `templates/bank/cmd/loan/main.go`
- `templates/bank/internal/wealth/domain/{money.go,wealth.go}` + `money_test.go` + `wealth_test.go`
- `templates/bank/internal/wealth/repo/wealth_repo.go` + `wealth_repo_test.go`（integration）
- `templates/bank/internal/wealth/service/wealth_service.go`
- `templates/bank/internal/wealth/api/{handlers.go,router.go}` + `handlers_test.go`
- `templates/bank/cmd/wealth/main.go`
- `templates/bank/internal/fixtures/domains/loan.go` + `loan_test.go`（纯单测）
- `templates/bank/internal/fixtures/domains/wealth.go` + `wealth_test.go`（纯单测）
- `templates/bank/internal/fixtures/wordlists_test.go`（词库 sanity）
- `templates/bank/internal/fixtures/domains/addmonths_test.go`（addMonths 单测）

**Modify:**
- `templates/bank/internal/fixtures/rng.go` — +LoanProducts/GuaranteeTypes/OverdueClasses/WealthProducts/OrderTypes/IncomeTypes 词库
- `templates/bank/internal/fixtures/domains/bizdate.go` — +`addMonths`
- `templates/bank/internal/platform/fdw/fdw.go` — `Mappings` +2 条
- `templates/bank/cmd/seed/main.go` — `allDBs` +2；新增 step8 loan / step9 wealth；日志 `x/8`→`x/10`
- `templates/bank/cmd/seed/seed_test.go` — 5 库名单→7（两处）+ 非空表清单 + B-4b 断言段
- `templates/bank/template.yaml` — +2 db +2 svc，version 0.4.0
- `templates/bank/docker-compose.yaml` — +loan(:8085) +wealth(:8086)
- `internal/template/manifest_test.go` — 断言 5→7（jiade 仓根）

**Repack:** jiade 根 `internal/template/templates.tar`（Task 12，`go generate` 产出）

## Execution Tracks（fan-out 用）

- **Shared（顺序，先做）**：Task 1（schema）→ Task 2（rng/bizdate/fdw 基建）。
- **Track L（loan，Task 2 后可并行）**：Task 3 → 4 → 5 → 6。文件域：`internal/loan/*`、`cmd/loan/*`、`fixtures/domains/loan*`。
- **Track W（wealth，Task 2 后可并行）**：Task 7 → 8 → 9 → 10。文件域：`internal/wealth/*`、`cmd/wealth/*`、`fixtures/domains/wealth*`。
- **Integration（两 Track 汇合后，顺序）**：Task 11（seed 编排）→ Task 12（template/compose/manifest/重打包/全量验证）。

> 注：Track L 与 Track W 文件零交集，可并行；两个 track 都依赖 Task 2 的词库与 `addMonths`。

---

### Task 1: loan_db + wealth_db schema

**Files:**
- Create: `templates/bank/db/migrations/loan_db.sql`
- Create: `templates/bank/db/migrations/wealth_db.sql`
- Test: `templates/bank/internal/platform/migrate/migrate_test.go`（追加一个测试函数）

**Interfaces:**
- Produces: 两个 schema 文件，被 Task 11 建表（`cmd/seed` 的 `migrate.Run`）、Task 12 `template.yaml` 引用；表结构是 Task 3/7 domain 与 Task 5/9 repo 的事实源。`loan_balance` PK `(loan_no,biz_date)`、`wealth_nav` PK `(product_code,biz_date)`，其余表 TEXT 主键。

- [ ] **Step 1: 创建 loan_db.sql**（DDL 列定义固定（零差异）；补索引，风格对齐 reward_db.sql 的 `CREATE INDEX IF NOT EXISTS idx_<表>_<列>`）

`templates/bank/db/migrations/loan_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS loan_product (
    product_code TEXT PRIMARY KEY,
    product_name TEXT NOT NULL,
    loan_type    TEXT NOT NULL,              -- 个人/对公/房贷/消费/经营
    rate_type    TEXT,
    min_rate     NUMERIC(10,6),
    max_rate     NUMERIC(10,6),
    max_term     INTEGER,
    max_amount   NUMERIC(18,2),
    status       TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS loan_account (
    loan_no         TEXT PRIMARY KEY,
    cust_id         TEXT NOT NULL,
    product_code    TEXT NOT NULL,
    ccy             TEXT NOT NULL,
    principal       NUMERIC(18,2) NOT NULL,
    balance         NUMERIC(18,2) NOT NULL,
    rate            NUMERIC(10,6) NOT NULL,
    start_biz_date  DATE NOT NULL,
    mature_date     DATE NOT NULL,
    term_months     INTEGER NOT NULL,
    status          TEXT DEFAULT 'disbursed',-- 放款/还款中/结清/逾期
    guarantee_type  TEXT,                    -- 信用/抵押/保证
    branch_code     TEXT
);
CREATE INDEX IF NOT EXISTS idx_loan_account_cust ON loan_account(cust_id);
CREATE INDEX IF NOT EXISTS idx_loan_account_product ON loan_account(product_code);

CREATE TABLE IF NOT EXISTS loan_disbursement (
    disb_id    TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    loan_no    TEXT NOT NULL,
    amount     NUMERIC(18,2) NOT NULL,
    to_account TEXT,
    disb_ts    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_loan_disb_bizdate ON loan_disbursement(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_disb_loan ON loan_disbursement(loan_no);

CREATE TABLE IF NOT EXISTS loan_repay (
    repay_id       TEXT PRIMARY KEY,
    biz_date       DATE NOT NULL,
    loan_no        TEXT NOT NULL,
    due_date       DATE NOT NULL,
    principal_amt  NUMERIC(18,2) NOT NULL,
    interest_amt   NUMERIC(18,2) NOT NULL,
    paid_principal NUMERIC(18,2) DEFAULT 0,
    paid_interest  NUMERIC(18,2) DEFAULT 0,
    status         TEXT DEFAULT 'open'       -- 未到期/已还/逾期
);
CREATE INDEX IF NOT EXISTS idx_loan_repay_bizdate ON loan_repay(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_repay_loan ON loan_repay(loan_no);

CREATE TABLE IF NOT EXISTS loan_overdue (
    overdue_id     TEXT PRIMARY KEY,
    biz_date       DATE NOT NULL,
    loan_no        TEXT NOT NULL,
    overdue_days   INTEGER NOT NULL,
    overdue_class  TEXT NOT NULL,            -- 正常/关注/次级/可疑/损失
    overdue_amount NUMERIC(18,2) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_bizdate ON loan_overdue(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_loan ON loan_overdue(loan_no);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_class ON loan_overdue(overdue_class);

CREATE TABLE IF NOT EXISTS loan_balance (
    loan_no              TEXT NOT NULL,
    biz_date             DATE NOT NULL,
    principal_balance    NUMERIC(18,2) NOT NULL,
    interest_receivable  NUMERIC(18,2) DEFAULT 0,
    PRIMARY KEY (loan_no, biz_date)
);
CREATE INDEX IF NOT EXISTS idx_loan_balance_bizdate ON loan_balance(biz_date);
```

- [ ] **Step 2: 创建 wealth_db.sql**（DDL 列定义固定 + 补索引）

`templates/bank/db/migrations/wealth_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS wealth_product (
    product_code     TEXT PRIMARY KEY,
    product_name     TEXT NOT NULL,
    product_type     TEXT NOT NULL,          -- 固收/权益/混合/货币/基金
    risk_level       TEXT,
    expected_return  NUMERIC(10,6),
    min_amount       NUMERIC(18,2),
    term_days        INTEGER,
    start_biz_date   DATE,
    end_biz_date     DATE,
    status           TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS wealth_nav (
    product_code TEXT NOT NULL,
    biz_date     DATE NOT NULL,
    nav          NUMERIC(12,6) NOT NULL,
    accum_nav    NUMERIC(12,6) NOT NULL,
    PRIMARY KEY (product_code, biz_date)
);
CREATE INDEX IF NOT EXISTS idx_wealth_nav_bizdate ON wealth_nav(biz_date);

CREATE TABLE IF NOT EXISTS wealth_holding (
    holding_id    TEXT PRIMARY KEY,
    cust_id       TEXT NOT NULL,
    account_no    TEXT NOT NULL,
    product_code  TEXT NOT NULL,
    ccy           TEXT NOT NULL,
    share         NUMERIC(18,4) NOT NULL,
    cost          NUMERIC(18,2) NOT NULL,
    current_value NUMERIC(18,2) NOT NULL,
    biz_date      DATE NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wealth_holding_cust ON wealth_holding(cust_id);
CREATE INDEX IF NOT EXISTS idx_wealth_holding_product ON wealth_holding(product_code);

CREATE TABLE IF NOT EXISTS wealth_order (
    order_id    TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    cust_id     TEXT NOT NULL,
    product_code TEXT NOT NULL,
    account_no  TEXT NOT NULL,
    order_type  TEXT NOT NULL,               -- 申购/赎回
    amount      NUMERIC(18,2),
    share       NUMERIC(18,4),
    nav         NUMERIC(12,6),
    status      TEXT DEFAULT 'done',
    order_ts    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_wealth_order_bizdate ON wealth_order(biz_date);
CREATE INDEX IF NOT EXISTS idx_wealth_order_cust ON wealth_order(cust_id);
CREATE INDEX IF NOT EXISTS idx_wealth_order_product ON wealth_order(product_code);

CREATE TABLE IF NOT EXISTS wealth_income (
    income_id   TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    holding_id  TEXT NOT NULL,
    income_type TEXT,                        -- 利息/分红
    amount      NUMERIC(18,2) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wealth_income_bizdate ON wealth_income(biz_date);
CREATE INDEX IF NOT EXISTS idx_wealth_income_holding ON wealth_income(holding_id);
```

- [ ] **Step 3: 扩展 migrate_test 断言新 schema 可切分**

在 `templates/bank/internal/platform/migrate/migrate_test.go` 末尾追加（该文件已 import `os`/`strings`/`testing`）：
```go
func TestSplitStatements_LoanWealthSchemas(t *testing.T) {
	for _, name := range []string{"loan_db.sql", "wealth_db.sql"} {
		// 3 级回到 templates/bank/（go test 的 CWD 是包目录 internal/platform/migrate/）。
		sql, err := os.ReadFile("../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !strings.Contains(s, "CREATE") {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}
```

- [ ] **Step 4: 跑测试**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/platform/migrate/...
```
Expected: PASS（含新旧 4 个 schema 切分测试）。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/db/migrations/loan_db.sql templates/bank/db/migrations/wealth_db.sql templates/bank/internal/platform/migrate/migrate_test.go
git commit -m "feat(bank): add loan_db + wealth_db schemas (B-4b)"
```

---

### Task 2: 共享基建——rng 词库 + addMonths + FDW Mappings

**Files:**
- Modify: `templates/bank/internal/fixtures/rng.go`
- Modify: `templates/bank/internal/fixtures/domains/bizdate.go`
- Modify: `templates/bank/internal/platform/fdw/fdw.go`
- Test: `templates/bank/internal/fixtures/wordlists_test.go`（新建）
- Test: `templates/bank/internal/fixtures/domains/addmonths_test.go`（新建）

**Interfaces:**
- Consumes: 既有 `fixtures.RNG`、`parseDate2`（bizdate.go）、fdw `Mapping` 结构。
- Produces（Task 4/8 依赖这些名字，逐字一致）:
  - `fixtures.LoanProducts []LoanProduct`，`LoanProduct{Code, Name, LoanType, CustType string; MinRate, MaxRate float64; MaxTerm int; MaxAmountYuan int}`
  - `fixtures.WealthProducts []WealthProduct`，`WealthProduct{Code, Name, Type, Risk string; ExpectedReturn float64; MinAmountYuan int; TermDays int}`
  - `fixtures.GuaranteeTypes []string`、`fixtures.OrderTypes []string`、`fixtures.IncomeTypes []string`
  - `fixtures.OverdueClasses []OverdueClass`，`OverdueClass{Days int; Name string}`
  - `addMonths(dateStr string, n int) string`（domains 包，bizdate.go）

- [ ] **Step 1: rng.go 加 B-4b 词库**

`templates/bank/internal/fixtures/rng.go` 的既有 `var ( ... )` 块内 `// B-4a 新增词库` 段末尾（`EntityTypes` 行后）追加：
```go
	// B-4b 新增词库
	GuaranteeTypes = []string{"信用", "抵押", "保证"}
	OrderTypes     = []string{"申购", "申购", "赎回"} // 2/3 申购
	IncomeTypes    = []string{"利息"}
```

`var` 块结束后追加产品元组类型与表（struct 不能在 var 块内）：
```go
// LoanProduct 贷款产品元组（CustType 仅元组保真，loan_product 表无此列）。
type LoanProduct struct {
	Code, Name, LoanType, CustType string
	MinRate, MaxRate               float64 // 年化比率（非金额）
	MaxTerm                        int     // 月
	MaxAmountYuan                  int     // 元（写库时 ×100 转分）
}

// LoanProducts 4 贷款产品。
var LoanProducts = []LoanProduct{
	{"LP-CONS", "个人消费贷", "消费", "个人", 0.0435, 0.0550, 36, 300000},
	{"LP-HOUS", "个人住房贷", "房贷", "个人", 0.0380, 0.0450, 360, 5000000},
	{"LP-OPER", "经营贷", "经营", "对公", 0.0450, 0.0600, 24, 2000000},
	{"LP-CRED", "信用贷", "消费", "个人", 0.0600, 0.0750, 12, 100000},
}

// OverdueClass 逾期五级分类档位（按逾期天数）。
type OverdueClass struct {
	Days int
	Name string
}

// OverdueClasses 5 档阈值表（天数升序）。
var OverdueClasses = []OverdueClass{{0, "正常"}, {1, "关注"}, {30, "次级"}, {90, "可疑"}, {180, "损失"}}

// WealthProduct 理财产品元组。
type WealthProduct struct {
	Code, Name, Type, Risk string
	ExpectedReturn         float64 // 年化比率（非金额）
	MinAmountYuan          int     // 元
	TermDays               int
}

// WealthProducts 6 理财产品。
var WealthProducts = []WealthProduct{
	{"WP-FIX1", "稳健固收1号", "固收", "低风险", 0.035, 1000, 365},
	{"WP-FIX3", "稳健固收3号", "固收", "中低", 0.040, 5000, 730},
	{"WP-MIX1", "平衡混合1号", "混合", "中", 0.065, 10000, 365},
	{"WP-EQT1", "成长股票1号", "权益", "中高", 0.085, 10000, 730},
	{"WP-MMO1", "现金管理1号", "货币", "低", 0.025, 100, 0},
	{"WP-FLX1", "灵活申赎1号", "货币", "低", 0.030, 1000, 0},
}
```

- [ ] **Step 2: bizdate.go 追加 addMonths**

`templates/bank/internal/fixtures/domains/bizdate.go` 的「日期 helper」段末尾追加（spec §6.2 逐字）：
```go
// addMonths 把 YYYY-MM-DD 加 n 月（n 可正可负；loan mature_date = start + term 用）。
func addMonths(dateStr string, n int) string {
	t, err := parseDate2(dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, n, 0).Format("2006-01-02")
}
```

- [ ] **Step 3: fdw.go Mappings +2**

`templates/bank/internal/platform/fdw/fdw.go`：把 `Mappings` 的注释 `覆盖 core/cust/pay/reward/risk 五库联邦` 改为 `覆盖 core/cust/pay/reward/risk/loan/wealth 七库联邦`，并在 slice 末尾追加两行：
```go
	{Host: "loan_db", Remote: "cust_db", Tables: []string{"cust_info"}},   // B-4b 新增
	{Host: "wealth_db", Remote: "cust_db", Tables: []string{"cust_info"}}, // B-4b 新增
```
`SetupFDW` 函数逻辑不变（自动遍历新 Mapping）。

- [ ] **Step 4: 写测试**

新建 `templates/bank/internal/fixtures/wordlists_test.go`:
```go
package fixtures

import "testing"

func TestB4bWordlists(t *testing.T) {
	if len(LoanProducts) != 4 {
		t.Errorf("LoanProducts 应 4 个, got %d", len(LoanProducts))
	}
	if LoanProducts[0].Code != "LP-CONS" || LoanProducts[1].MaxAmountYuan != 5000000 {
		t.Errorf("LoanProducts 元组错: %+v", LoanProducts[0])
	}
	if len(WealthProducts) != 6 {
		t.Errorf("WealthProducts 应 6 个, got %d", len(WealthProducts))
	}
	if WealthProducts[0].Code != "WP-FIX1" || WealthProducts[5].MinAmountYuan != 1000 {
		t.Errorf("WealthProducts 元组错: %+v", WealthProducts[5])
	}
	if len(OverdueClasses) != 5 {
		t.Fatalf("OverdueClasses 应 5 档, got %d", len(OverdueClasses))
	}
	for i := 1; i < len(OverdueClasses); i++ {
		if OverdueClasses[i].Days <= OverdueClasses[i-1].Days {
			t.Errorf("OverdueClasses 天数应升序: %+v", OverdueClasses)
		}
	}
	if OverdueClasses[4].Name != "损失" {
		t.Errorf("第 5 档应为损失, got %s", OverdueClasses[4].Name)
	}
	if len(GuaranteeTypes) != 3 || len(OrderTypes) != 3 || len(IncomeTypes) != 1 {
		t.Error("GuaranteeTypes/OrderTypes/IncomeTypes 长度错")
	}
	if OrderTypes[0] != "申购" || OrderTypes[2] != "赎回" {
		t.Errorf("OrderTypes 错: %v", OrderTypes)
	}
}
```

新建 `templates/bank/internal/fixtures/domains/addmonths_test.go`:
```go
package domains

import "testing"

func TestAddMonths(t *testing.T) {
	cases := []struct{ in string; n int; want string }{
		{"2025-06-15", 12, "2026-06-15"},
		{"2025-06-15", 24, "2027-06-15"},
		{"2025-06-15", 1, "2025-07-15"},
		{"2026-07-13", -1, "2026-06-13"},
		{"2026-07-13", -2, "2026-05-13"},
		{"bad-date", 3, "bad-date"}, // 解析失败原样返回
	}
	for _, c := range cases {
		if got := addMonths(c.in, c.n); got != c.want {
			t.Errorf("addMonths(%s,%d)=%s want %s", c.in, c.n, got, c.want)
		}
	}
}
```

- [ ] **Step 5: 跑测试 + build**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
```
Expected: PASS + build 成功。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/fixtures/rng.go templates/bank/internal/fixtures/wordlists_test.go templates/bank/internal/fixtures/domains/bizdate.go templates/bank/internal/fixtures/domains/addmonths_test.go templates/bank/internal/platform/fdw/fdw.go
git commit -m "feat(bank): B-4b 共享基建——loan/wealth 词库 + addMonths + FDW Mappings"
```

---

### Task 3: loan domain（Money 副本 + 6 表模型 + LoanProfile）

**Files:**
- Create: `templates/bank/internal/loan/domain/money.go`
- Create: `templates/bank/internal/loan/domain/loan.go`
- Test: `templates/bank/internal/loan/domain/money_test.go`
- Test: `templates/bank/internal/loan/domain/loan_test.go`

**Interfaces:**
- Consumes: 无（最内层，零 DB/框架依赖）。
- Produces（Task 4/5/6 依赖，逐字一致）: `Money`/`NewMoneyFromCents(int64) Money`/`ParseCents(string) (Money, error)`/`(Money) Add/Sub/Cents/String`；结构体 `LoanProduct`/`LoanAccount`/`LoanDisbursement`/`LoanRepay`/`LoanOverdue`/`LoanBalance`/`LoanProfile`（字段名与类型见 Step 2 代码）。

- [ ] **Step 1: 复制 Money**

把 `templates/bank/internal/reward/domain/money.go` **逐字复制**为 `templates/bank/internal/loan/domain/money.go`（61 行，包名同为 `domain`，零改动）：
```bash
cp templates/bank/internal/reward/domain/money.go templates/bank/internal/loan/domain/money.go
```

同样复制其测试：
```bash
cp templates/bank/internal/reward/domain/money_test.go templates/bank/internal/loan/domain/money_test.go
```

- [ ] **Step 2: 写 loan.go 领域模型**

`templates/bank/internal/loan/domain/loan.go`:
```go
// Package domain 是 loan 服务的纯领域模型，零 DB/框架依赖（最内层）。
// 金额字段用 Money（int64 分）；rate/min_rate/max_rate 是 NUMERIC(10,6) 比率（非金额），文本直存。
package domain

// LoanProduct 对应 loan_product 表。
type LoanProduct struct {
	ProductCode string
	ProductName string
	LoanType    string
	RateType    string
	MinRate     string // NUMERIC(10,6) 文本（比率，非金额）
	MaxRate     string
	MaxTerm     int
	MaxAmount   Money
	Status      string
}

// LoanAccount 对应 loan_account 表。
type LoanAccount struct {
	LoanNo        string
	CustID        string
	ProductCode   string
	Ccy           string
	Principal     Money
	Balance       Money
	Rate          string // NUMERIC(10,6) 文本（比率，非金额）
	StartBizDate  string
	MatureDate    string
	TermMonths    int
	Status        string
	GuaranteeType string
	BranchCode    string
}

// LoanDisbursement 对应 loan_disbursement 表。
type LoanDisbursement struct {
	DisbID    string
	BizDate   string
	LoanNo    string
	Amount    Money
	ToAccount string
}

// LoanRepay 对应 loan_repay 表。
type LoanRepay struct {
	RepayID       string
	BizDate       string
	LoanNo        string
	DueDate       string
	PrincipalAmt  Money
	InterestAmt   Money
	PaidPrincipal Money
	PaidInterest  Money
	Status        string
}

// LoanOverdue 对应 loan_overdue 表。
type LoanOverdue struct {
	OverdueID     string
	BizDate       string
	LoanNo        string
	OverdueDays   int
	OverdueClass  string
	OverdueAmount Money
}

// LoanBalance 对应 loan_balance 表（逐日全量快照）。
type LoanBalance struct {
	LoanNo             string
	BizDate            string
	PrincipalBalance   Money
	InterestReceivable Money
}

// LoanProfile 是联邦查询结果（loan_account JOIN ext_cust_db_cust_info）。
type LoanProfile struct {
	LoanNo    string
	CustID    string
	Principal Money
	Balance   Money
	Rate      string
	Status    string
	CustName  string
	CustType  string
}
```

- [ ] **Step 3: 写 loan_test.go**

`templates/bank/internal/loan/domain/loan_test.go`:
```go
package domain

import "testing"

func TestLoanAccount(t *testing.T) {
	a := LoanAccount{LoanNo: "LN0000000", CustID: "C0000001", Principal: NewMoneyFromCents(1000000), Balance: NewMoneyFromCents(1000000), Rate: "0.043500"}
	if a.LoanNo != "LN0000000" || a.Principal.String() != "10000.00" {
		t.Errorf("got %+v", a)
	}
}

func TestLoanRepayMoneyRoundTrip(t *testing.T) {
	r := LoanRepay{RepayID: "LN-RP-20250601-00000", PrincipalAmt: NewMoneyFromCents(83333), InterestAmt: NewMoneyFromCents(3625)}
	if r.PrincipalAmt.String() != "833.33" || r.InterestAmt.String() != "36.25" {
		t.Errorf("repay 金额错: %s %s", r.PrincipalAmt, r.InterestAmt)
	}
	p, err := ParseCents("833.33")
	if err != nil || p != r.PrincipalAmt {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}
```

- [ ] **Step 4: 跑测试**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/loan/domain/...
```
Expected: PASS（TestMoneyRoundTrip / TestSourceHasNoFloat / TestLoanAccount / TestLoanRepayMoneyRoundTrip）。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/loan/domain/
git commit -m "feat(bank): loan domain——Money 副本 + 6 表模型 (B-4b)"
```

---

### Task 4: loan fixture 生成器（静态 + 逐日滚存，无三因子）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/loan.go`
- Test: `templates/bank/internal/fixtures/domains/loan_test.go`

**Interfaces:**
- Consumes: Task 2 的 `fixtures.LoanProducts`/`GuaranteeTypes`/`OverdueClasses` + `addMonths`；既有 `fixtures.NewRNG/RandomDate`、`bizDateRange/dayOrdinal/dateCompact/placeholders/nullable/maxInt/minInt/pickStr`（同包）、`bizDateBatchSize`（同包）、`pg.RunInTx/DBTX`；Task 3 的 `bank/internal/loan/domain`。
- Produces（Task 11 依赖，逐字一致）:
  - `type LoanStatic struct { Products []domain.LoanProduct; Accounts []domain.LoanAccount; Disbursements []domain.LoanDisbursement }`
  - `GenLoanStatic(cfg fixtures.Config, custIDs []string) LoanStatic`
  - `WriteLoanStatic(ctx context.Context, db *sql.DB, s LoanStatic) error`
  - `RunLoan(ctx context.Context, db *sql.DB, cfg fixtures.Config, accounts []domain.LoanAccount) error`

- [ ] **Step 1: 写 loan.go**

`templates/bank/internal/fixtures/domains/loan.go`（rng `seed+40` 静态 / `seed+41` 滚存单次；确定性 ID，无 uuid；公式忠实还原）:
```go
package domains

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	"bank/internal/fixtures"
	"bank/internal/loan/domain"
	"bank/internal/platform/pg"
)

// LoanStatic 静态表行集合（一次性生成）。
type LoanStatic struct {
	Products      []domain.LoanProduct
	Accounts      []domain.LoanAccount
	Disbursements []domain.LoanDisbursement
}

// GenLoanStatic 生成贷款产品/借据/放款（rng seed+40；产品固定 4 行，借据数从 custIDs 派生，零 Counts 改动）。
func GenLoanStatic(cfg fixtures.Config, custIDs []string) LoanStatic {
	rng := fixtures.NewRNG(cfg.Seed + 40)
	products := make([]domain.LoanProduct, len(fixtures.LoanProducts))
	for i, p := range fixtures.LoanProducts {
		products[i] = domain.LoanProduct{
			ProductCode: p.Code, ProductName: p.Name, LoanType: p.LoanType, RateType: "fixed",
			MinRate: fmt.Sprintf("%.6f", p.MinRate), MaxRate: fmt.Sprintf("%.6f", p.MaxRate),
			MaxTerm: p.MaxTerm, MaxAmount: domain.NewMoneyFromCents(int64(p.MaxAmountYuan) * 100),
			Status: "active",
		}
	}
	nLoans := maxInt(5, len(custIDs)/4)
	accounts := make([]domain.LoanAccount, 0, nLoans)
	disbs := make([]domain.LoanDisbursement, 0, nLoans)
	for i := 0; i < nLoans; i++ {
		cid := pickStr(rng, custIDs) // 抽签顺序固定：cust → product → principal → rate → term → start → guarantee/branch → to_account
		p := fixtures.LoanProducts[rng.IntRange(0, len(fixtures.LoanProducts)-1)]
		// 公式：IntRange(0,99999)×(maxAmtYuan/100000) 元，clamp 到 [10000, maxAmtYuan]（纯整数）
		principalYuan := rng.IntRange(0, 99999) * (p.MaxAmountYuan / 100000)
		principalYuan = maxInt(10000, minInt(principalYuan, p.MaxAmountYuan))
		principal := domain.NewMoneyFromCents(int64(principalYuan) * 100)
		rate := p.MinRate + rng.Float64()*(p.MaxRate-p.MinRate) // 比率（非金额），float 可接受
		term := []int{12, 24}[rng.IntRange(0, 1)]
		if p.MaxTerm >= 36 {
			term = []int{12, 24, 36}[rng.IntRange(0, 2)]
		}
		start := fixtures.RandomDate(rng, cfg.StartBizDate, maxDateStr(cfg.StartBizDate, addMonths(cfg.EndBizDate, -1))) // 短区间守卫
		loanNo := fmt.Sprintf("LN%07d", i)
		accounts = append(accounts, domain.LoanAccount{
			LoanNo: loanNo, CustID: cid, ProductCode: p.Code, Ccy: "CNY",
			Principal: principal, Balance: principal, Rate: fmt.Sprintf("%.6f", rate),
			StartBizDate: start, MatureDate: addMonths(start, term), TermMonths: term,
			Status: "disbursed", GuaranteeType: rng.Choice(fixtures.GuaranteeTypes),
			BranchCode: rng.Choice(fixtures.Branches),
		})
		disbs = append(disbs, domain.LoanDisbursement{
			DisbID: fmt.Sprintf("LN-DB-%07d", i), BizDate: start, LoanNo: loanNo,
			Amount: principal, ToAccount: fmt.Sprintf("D%010d", rng.IntRange(0, 9999999999)),
		})
	}
	return LoanStatic{Products: products, Accounts: accounts, Disbursements: disbs}
}

// WriteLoanStatic 幂等写 loan_product/loan_account/loan_disbursement（先 DELETE 后 INSERT）。
func WriteLoanStatic(ctx context.Context, db *sql.DB, s LoanStatic) error {
	for _, t := range []string{"loan_disbursement", "loan_account", "loan_product"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range s.Products {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_product(product_code,product_name,loan_type,rate_type,min_rate,max_rate,max_term,max_amount,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			p.ProductCode, p.ProductName, p.LoanType, p.RateType, p.MinRate, p.MaxRate,
			p.MaxTerm, p.MaxAmount.String(), p.Status); err != nil {
			return err
		}
	}
	for _, a := range s.Accounts {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_account(loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			a.LoanNo, a.CustID, a.ProductCode, a.Ccy, a.Principal.String(), a.Balance.String(), a.Rate,
			a.StartBizDate, a.MatureDate, a.TermMonths, a.Status, a.GuaranteeType, a.BranchCode); err != nil {
			return err
		}
	}
	for _, d := range s.Disbursements {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_disbursement(disb_id,biz_date,loan_no,amount,to_account,disb_ts)
			VALUES($1,$2,$3,$4,$5,CURRENT_TIMESTAMP)`,
			d.DisbID, d.BizDate, d.LoanNo, d.Amount.String(), d.ToAccount); err != nil {
			return err
		}
	}
	return nil
}

// loanState 借据滚存状态（内存；balance 跨日滚存，路径依赖）。
type loanState struct {
	balance          domain.Money
	overdueDays      int
	overdueStart     string // 空 = 不逾期
	monthlyPrincipal domain.Money
	rateFloat        float64
}

// RunLoan 按 bizDate 推进：月初还款计划 + 逾期五级分类滑落 + 每日全量余额快照。
// rng seed+41 单次（无逐日随机）；每业务日一个 pg.RunInTx。全量重放确定。
func RunLoan(ctx context.Context, db *sql.DB, cfg fixtures.Config, accounts []domain.LoanAccount) error {
	if len(accounts) == 0 {
		return fmt.Errorf("loan: 无借据")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("loan: %w", err)
	}
	rng := fixtures.NewRNG(cfg.Seed + 41)
	state := make(map[string]*loanState, len(accounts))
	for _, a := range accounts {
		rateF, _ := strconv.ParseFloat(a.Rate, 64)
		state[a.LoanNo] = &loanState{
			balance:          a.Balance,
			monthlyPrincipal: domain.NewMoneyFromCents(roundDiv(a.Principal.Cents(), int64(a.TermMonths))),
			rateFloat:        rateF,
		}
	}
	// 逾期选择：~8%（random_int(1,12)==1 口径），overdue_start ∈ [start, max(start, end-2月)]
	for _, a := range accounts {
		if rng.IntRange(1, 12) == 1 {
			state[a.LoanNo].overdueStart = fixtures.RandomDate(rng, cfg.StartBizDate, maxDateStr(cfg.StartBizDate, addMonths(cfg.EndBizDate, -2)))
		}
	}
	lastMonth := time.Month(0)
	for _, d := range days {
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		// 月初（月份翻转）：对 balance>0 借据造当月还款计划
		var repays []domain.LoanRepay
		monthStart := d.Month() != lastMonth
		if monthStart {
			lastMonth = d.Month()
			for i, a := range accounts {
				st := state[a.LoanNo]
				if st.balance <= 0 {
					continue
				}
				principalAmt := minMoney(st.monthlyPrincipal, st.balance)
				interestAmt := domain.NewMoneyFromCents(int64(math.Round(float64(st.balance.Cents()) * st.rateFloat / 12)))
				r := domain.LoanRepay{
					RepayID: fmt.Sprintf("LN-RP-%s-%05d", compact, i),
					BizDate: dateStr, LoanNo: a.LoanNo, DueDate: dateStr,
					PrincipalAmt: principalAmt, InterestAmt: interestAmt,
				}
				if st.overdueStart != "" && dateStr >= st.overdueStart {
					r.Status = "overdue" // 逾期不扣款，余额不动
				} else {
					st.balance = st.balance.Sub(principalAmt)
					if st.balance < 0 {
						st.balance = 0
					}
					r.PaidPrincipal, r.PaidInterest = principalAmt, interestAmt
					r.Status = "open"
				}
				repays = append(repays, r)
			}
		}
		// 累计逾期天数（ISO 日期字典序可比较）
		for _, a := range accounts {
			st := state[a.LoanNo]
			if st.overdueStart != "" && dateStr > st.overdueStart {
				st.overdueDays = int(dayOrdinal(d, parseDate(st.overdueStart)))
			}
		}
		// 当日全量快照 + 逾期滑落
		var balances []domain.LoanBalance
		var overdues []domain.LoanOverdue
		for _, a := range accounts {
			st := state[a.LoanNo]
			if st.balance > 0 {
				balances = append(balances, domain.LoanBalance{
					LoanNo: a.LoanNo, BizDate: dateStr, PrincipalBalance: st.balance,
					InterestReceivable: domain.NewMoneyFromCents(int64(math.Round(float64(st.balance.Cents()) * st.rateFloat / 360))),
				})
			}
			if st.overdueDays > 0 && st.overdueStart != "" && dateStr > st.overdueStart {
				overdues = append(overdues, domain.LoanOverdue{
					OverdueID: fmt.Sprintf("LN-OD-%s-%s", compact, a.LoanNo),
					BizDate: dateStr, LoanNo: a.LoanNo, OverdueDays: st.overdueDays,
					OverdueClass: overdueClass(st.overdueDays), OverdueAmount: st.balance,
				})
			}
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if monthStart {
				if _, err := q.ExecContext(ctx, "DELETE FROM loan_repay WHERE biz_date=$1", dateStr); err != nil {
					return fmt.Errorf("删当日 loan_repay %s: %w", dateStr, err)
				}
				if err := bulkInsertLoanRepays(ctx, q, repays); err != nil {
					return err
				}
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM loan_balance WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 loan_balance %s: %w", dateStr, err)
			}
			if err := bulkInsertLoanBalances(ctx, q, balances); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM loan_overdue WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 loan_overdue %s: %w", dateStr, err)
			}
			return bulkInsertLoanOverdues(ctx, q, overdues)
		}); err != nil {
			return fmt.Errorf("loan: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertLoanRepays 批量插 loan_repay（9 列）。
func bulkInsertLoanRepays(ctx context.Context, q pg.DBTX, rows []domain.LoanRepay) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 9
	const stmt = "INSERT INTO loan_repay(repay_id,biz_date,loan_no,due_date,principal_amt,interest_amt,paid_principal,paid_interest,status) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.RepayID, r.BizDate, r.LoanNo, r.DueDate,
				r.PrincipalAmt.String(), r.InterestAmt.String(),
				r.PaidPrincipal.String(), r.PaidInterest.String(), r.Status)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_repay: %w", err)
		}
	}
	return nil
}

// bulkInsertLoanBalances 批量插 loan_balance（4 列）。
func bulkInsertLoanBalances(ctx context.Context, q pg.DBTX, rows []domain.LoanBalance) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 4
	const stmt = "INSERT INTO loan_balance(loan_no,biz_date,principal_balance,interest_receivable) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, b := range chunk {
			args = append(args, b.LoanNo, b.BizDate, b.PrincipalBalance.String(), b.InterestReceivable.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_balance: %w", err)
		}
	}
	return nil
}

// bulkInsertLoanOverdues 批量插 loan_overdue（6 列）。
func bulkInsertLoanOverdues(ctx context.Context, q pg.DBTX, rows []domain.LoanOverdue) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 6
	const stmt = "INSERT INTO loan_overdue(overdue_id,biz_date,loan_no,overdue_days,overdue_class,overdue_amount) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, o := range chunk {
			args = append(args, o.OverdueID, o.BizDate, o.LoanNo, o.OverdueDays, o.OverdueClass, o.OverdueAmount.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_overdue: %w", err)
		}
	}
	return nil
}

// overdueClass 按逾期天数划五级分类。
func overdueClass(days int) string {
	cls := "正常"
	for _, oc := range fixtures.OverdueClasses {
		if days >= oc.Days {
			cls = oc.Name
		}
	}
	return cls
}

// roundDiv 四舍五入整数除法（a/b，a 非负）。
func roundDiv(a, b int64) int64 {
	return (a + b/2) / b
}

// minMoney 较小金额。
func minMoney(a, b domain.Money) domain.Money {
	if a < b {
		return a
	}
	return b
}

// maxDateStr 返回两个 YYYY-MM-DD 中较大者（ISO 字典序即时间序）。
func maxDateStr(a, b string) string {
	if b > a {
		return b
	}
	return a
}
```

- [ ] **Step 2: 写 loan_test.go（确定性 + 档位 + helper）**

`templates/bank/internal/fixtures/domains/loan_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenLoanStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003", "C0000004", "C0000005", "C0000006", "C0000007", "C0000008"}
	a := GenLoanStatic(cfg, custIDs)
	b := GenLoanStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenLoanStatic 不确定")
	}
	if len(a.Products) != 4 {
		t.Errorf("产品应 4 个, got %d", len(a.Products))
	}
	if len(a.Accounts) != maxInt(5, len(custIDs)/4) { // 8/4=2 → maxInt(5,2)=5
		t.Errorf("借据数=%d, want 5", len(a.Accounts))
	}
	if len(a.Disbursements) != len(a.Accounts) {
		t.Errorf("放款数应=借据数")
	}
	acct := a.Accounts[0]
	if acct.LoanNo != "LN0000000" {
		t.Errorf("loan_no=%s want LN0000000", acct.LoanNo)
	}
	if acct.Principal.Cents() < 10000*100 {
		t.Errorf("本金应 ≥10000 元, got %d 分", acct.Principal.Cents())
	}
	if acct.Balance != acct.Principal {
		t.Error("初始余额应=本金")
	}
	if acct.MatureDate != addMonths(acct.StartBizDate, acct.TermMonths) {
		t.Errorf("mature=%s 应=start+term", acct.MatureDate)
	}
	if a.Disbursements[0].DisbID != "LN-DB-0000000" || a.Disbursements[0].Amount != acct.Principal {
		t.Errorf("放款错: %+v", a.Disbursements[0])
	}
	if len(acct.Rate) != 8 { // "0.043500" 6dp
		t.Errorf("rate 应 6dp 文本, got %q", acct.Rate)
	}
}

func TestOverdueClass(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		{0, "正常"}, {1, "关注"}, {29, "关注"}, {30, "次级"}, {89, "次级"},
		{90, "可疑"}, {179, "可疑"}, {180, "损失"}, {365, "损失"},
	}
	for _, c := range cases {
		if got := overdueClass(c.days); got != c.want {
			t.Errorf("overdueClass(%d)=%s want %s", c.days, got, c.want)
		}
	}
}

func TestRoundDiv(t *testing.T) {
	if roundDiv(100, 12) != 8 { // 8.33→8
		t.Errorf("roundDiv(100,12)=%d want 8", roundDiv(100, 12))
	}
	if roundDiv(101, 2) != 51 { // 50.5→51
		t.Errorf("roundDiv(101,2)=%d want 51", roundDiv(101, 2))
	}
}

func TestMaxDateStr(t *testing.T) {
	if maxDateStr("2025-06-01", "2026-06-13") != "2026-06-13" {
		t.Error("maxDateStr 应取大者")
	}
	if maxDateStr("2026-07-13", "2025-01-01") != "2026-07-13" {
		t.Error("maxDateStr 短区间守卫")
	}
}
```

- [ ] **Step 3: 跑测试 + build**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
```
Expected: PASS + build 成功。

- [ ] **Step 4: Commit**
```bash
git add templates/bank/internal/fixtures/domains/loan.go templates/bank/internal/fixtures/domains/loan_test.go
git commit -m "feat(bank): loan fixture 生成器——静态 + 逐日滚存/逾期滑落 (B-4b)"
```

---

### Task 5: loan repo（本库查询 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/loan/repo/loan_repo.go`
- Test: `templates/bank/internal/loan/repo/loan_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: Task 3 的 `bank/internal/loan/domain`；`bank/internal/platform/pg` 的 `*sql.DB`；Task 1 的表结构；Task 2 的 FDW 外部表 `ext_cust_db_cust_info`（列：`cust_id,name,cust_type,...`，镜像 risk 用法）。
- Produces（Task 6 依赖，逐字一致）:
  - `type LoanRepo struct{ db *sql.DB }` + `NewLoanRepo(db *sql.DB) *LoanRepo`
  - `ListProducts(ctx context.Context) ([]domain.LoanProduct, error)`
  - `ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error)`
  - `GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error)`
  - `ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error)`
  - `ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error)`
  - `GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error)`

- [ ] **Step 1: 写 loan_repo.go**

`templates/bank/internal/loan/repo/loan_repo.go`（DATE 直扫 string；NUMERIC 金额 → NullString → `domain.ParseCents`；rate → NullString 直存；404 语义 = 包装 `sql.ErrNoRows`）:
```go
// Package repo 是 loan 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/loan/domain"
)

// LoanRepo loan 仓储。本库 loan_* 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type LoanRepo struct{ db *sql.DB }

// NewLoanRepo 构造 LoanRepo。
func NewLoanRepo(db *sql.DB) *LoanRepo { return &LoanRepo{db: db} }

// ListProducts 列贷款产品（静态全量）。
func (r *LoanRepo) ListProducts(ctx context.Context) ([]domain.LoanProduct, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,product_name,loan_type,rate_type,min_rate,max_rate,max_term,max_amount,status
		FROM loan_product ORDER BY product_code`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列贷款产品: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanProduct
	for rows.Next() {
		var p domain.LoanProduct
		var rateType, minRate, maxRate, maxAmt, status sql.NullString
		if err := rows.Scan(&p.ProductCode, &p.ProductName, &p.LoanType, &rateType, &minRate, &maxRate, &p.MaxTerm, &maxAmt, &status); err != nil {
			return nil, fmt.Errorf("repo: 列贷款产品 scan: %w", err)
		}
		p.RateType, p.MinRate, p.MaxRate, p.Status = rateType.String, minRate.String, maxRate.String, status.String
		m, err := domain.ParseCents(maxAmt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析产品额度: %w", err)
		}
		p.MaxAmount = m
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列贷款产品: %w", err)
	}
	return out, nil
}

// ListAccounts 按产品/状态筛选借据（空则不限），分页。limit<=0 取 50。
func (r *LoanRepo) ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code
		FROM loan_account WHERE ($1='' OR product_code=$1) AND ($2='' OR status=$2)
		ORDER BY loan_no LIMIT $3 OFFSET $4`, productCode, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列借据: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanAccount
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列借据 scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列借据: %w", err)
	}
	return out, nil
}

// GetAccount 查单个借据。不存在返回包装 sql.ErrNoRows。
func (r *LoanRepo) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	a, err := scanAccount(r.db.QueryRowContext(ctx,
		`SELECT loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code
		FROM loan_account WHERE loan_no=$1`, loanNo).Scan)
	if err != nil {
		return domain.LoanAccount{}, fmt.Errorf("repo: 查借据 %s: %w", loanNo, err)
	}
	return a, nil
}

// ListBalances 按日期范围/借据查逐日余额快照（空则不限），分页。
func (r *LoanRepo) ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT loan_no,biz_date,principal_balance,interest_receivable FROM loan_balance
		WHERE (NULLIF($1,'') IS NULL OR biz_date >= NULLIF($1,'')::date)
		AND (NULLIF($2,'') IS NULL OR biz_date <= NULLIF($2,'')::date)
		AND ($3='' OR loan_no=$3)
		ORDER BY biz_date DESC, loan_no LIMIT $4 OFFSET $5`, from, to, loanNo, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列借据余额: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanBalance
	for rows.Next() {
		var b domain.LoanBalance
		var pb, ir sql.NullString
		if err := rows.Scan(&b.LoanNo, &b.BizDate, &pb, &ir); err != nil {
			return nil, fmt.Errorf("repo: 列借据余额 scan: %w", err)
		}
		var err error
		if b.PrincipalBalance, err = domain.ParseCents(pb.String); err != nil {
			return nil, fmt.Errorf("repo: 解析本金余额: %w", err)
		}
		if b.InterestReceivable, err = domain.ParseCents(ir.String); err != nil {
			return nil, fmt.Errorf("repo: 解析应收利息: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列借据余额: %w", err)
	}
	return out, nil
}

// ListOverdue 按五级分类/日期范围查逾期（空则不限），分页。
func (r *LoanRepo) ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT overdue_id,biz_date,loan_no,overdue_days,overdue_class,overdue_amount FROM loan_overdue
		WHERE ($1='' OR overdue_class=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date DESC, loan_no LIMIT $4 OFFSET $5`, overdueClass, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列逾期: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanOverdue
	for rows.Next() {
		var o domain.LoanOverdue
		var amt sql.NullString
		if err := rows.Scan(&o.OverdueID, &o.BizDate, &o.LoanNo, &o.OverdueDays, &o.OverdueClass, &amt); err != nil {
			return nil, fmt.Errorf("repo: 列逾期 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析逾期金额: %w", err)
		}
		o.OverdueAmount = m
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列逾期: %w", err)
	}
	return out, nil
}

// GetProfile 跨库联邦：loan_account JOIN ext_cust_db_cust_info → 借据本金/余额 + 客户姓名/类型。
func (r *LoanRepo) GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	var p domain.LoanProfile
	var principal, balance, rate, status, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT la.loan_no, la.cust_id, la.principal, la.balance, la.rate, la.status, ci.name, ci.cust_type
		FROM loan_account la
		LEFT JOIN ext_cust_db_cust_info ci ON la.cust_id=ci.cust_id
		WHERE la.loan_no=$1`, loanNo).
		Scan(&p.LoanNo, &p.CustID, &principal, &balance, &rate, &status, &name, &ctype)
	if err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 联邦查借据档案 %s: %w", loanNo, err)
	}
	if p.Principal, err = domain.ParseCents(principal.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据本金: %w", err)
	}
	if p.Balance, err = domain.ParseCents(balance.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据余额: %w", err)
	}
	p.Rate, p.Status, p.CustName, p.CustType = rate.String, status.String, name.String, ctype.String
	return p, nil
}

// scanAccount 扫描单行 loan_account（scan 函数由 QueryRow 或 Rows 注入）。
func scanAccount(scan func(dest ...any) error) (domain.LoanAccount, error) {
	var a domain.LoanAccount
	var principal, balance, rate, guarantee, branch sql.NullString
	if err := scan(&a.LoanNo, &a.CustID, &a.ProductCode, &a.Ccy, &principal, &balance, &rate,
		&a.StartBizDate, &a.MatureDate, &a.TermMonths, &a.Status, &guarantee, &branch); err != nil {
		return domain.LoanAccount{}, err
	}
	var err error
	if a.Principal, err = domain.ParseCents(principal.String); err != nil {
		return domain.LoanAccount{}, fmt.Errorf("解析本金: %w", err)
	}
	if a.Balance, err = domain.ParseCents(balance.String); err != nil {
		return domain.LoanAccount{}, fmt.Errorf("解析余额: %w", err)
	}
	a.Rate, a.GuaranteeType, a.BranchCode = rate.String, guarantee.String, branch.String
	return a, nil
}
```

- [ ] **Step 2: 写 loan_repo_test.go（integration）**

`templates/bank/internal/loan/repo/loan_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/loan/repo"
	"bank/internal/platform/pg"
)

func setupLoanDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("loan_db")
	if err != nil {
		t.Skipf("无 loan_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestLoanRepo_ListProducts(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	prods, err := repo.NewLoanRepo(db).ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != 4 {
		t.Errorf("贷款产品应 4 个, got %d", len(prods))
	}
}

func TestLoanRepo_ListsAndDetail(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewLoanRepo(db)
	if _, err := r.ListAccounts(ctx, "", "", 0, 10); err != nil {
		t.Fatalf("ListAccounts 失败: %v", err)
	}
	if _, err := r.ListBalances(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListBalances 失败: %v", err)
	}
	if _, err := r.ListOverdue(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOverdue 失败: %v", err)
	}
	if _, err := r.GetAccount(ctx, "LN-NOPE"); err == nil {
		t.Error("不存在的借据应返回错误")
	}
}

func TestLoanRepo_GetProfile_FDWJoin(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err := repo.NewLoanRepo(db).GetProfile(context.Background(), "LN-NOPE")
	if err == nil {
		t.Error("不存在的借据应返回错误")
	}
}
```

- [ ] **Step 3: build + 非集成测试可编译**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go vet ./internal/loan/...
```
Expected: 成功。（integration 测试在 Task 12 统一跑真 pg。）

- [ ] **Step 4: Commit**
```bash
git add templates/bank/internal/loan/repo/
git commit -m "feat(bank): loan repo——本库查询 + FDW 联邦 JOIN (B-4b)"
```

---

### Task 6: loan service + api + cmd

**Files:**
- Create: `templates/bank/internal/loan/service/loan_service.go`
- Create: `templates/bank/internal/loan/api/router.go`
- Create: `templates/bank/internal/loan/api/handlers.go`
- Test: `templates/bank/internal/loan/api/handlers_test.go`
- Create: `templates/bank/cmd/loan/main.go`

**Interfaces:**
- Consumes: Task 3 domain、Task 5 repo（`NewLoanRepo` + 6 个方法签名）。
- Produces: `service.LoanStore` 接口（= Task 5 的 6 个方法签名逐字）+ `NewLoanService(store LoanStore) *LoanService`；api `Handlers{Svc *service.LoanService}` + `NewRouter(h *Handlers) http.Handler`；`cmd/loan` 可构建入口（compose `CMD: loan` 引用，Task 12）。
- 端点（spec §9）: `GET /healthz`、`GET /api/v1/loan/products`、`GET /api/v1/loan/accounts`、`GET /api/v1/loan/accounts/{loan_no}`、`GET /api/v1/loan/balances`、`GET /api/v1/loan/overdue`、`GET /api/v1/loan/accounts/{loan_no}/profile`。

- [ ] **Step 1: 写 loan_service.go（薄封装）**

`templates/bank/internal/loan/service/loan_service.go`:
```go
// Package service 是 loan 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/loan/domain"
)

// LoanStore loan 查询接口（repo 实现）。
type LoanStore interface {
	ListProducts(ctx context.Context) ([]domain.LoanProduct, error)
	ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error)
	GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error)
	ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error)
	ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error)
	GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error)
}

// LoanService loan 只读服务，包装 LoanStore 做查询编排。
type LoanService struct{ store LoanStore }

// NewLoanService 构造 LoanService。
func NewLoanService(store LoanStore) *LoanService { return &LoanService{store: store} }

// ListProducts 列贷款产品。
func (s *LoanService) ListProducts(ctx context.Context) ([]domain.LoanProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListAccounts 按产品/状态筛选借据并分页。
func (s *LoanService) ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	return s.store.ListAccounts(ctx, productCode, status, offset, limit)
}

// GetAccount 查单个借据。
func (s *LoanService) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	return s.store.GetAccount(ctx, loanNo)
}

// ListBalances 按日期范围查逐日余额快照。
func (s *LoanService) ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error) {
	return s.store.ListBalances(ctx, from, to, loanNo, offset, limit)
}

// ListOverdue 按五级分类/日期范围查逾期。
func (s *LoanService) ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error) {
	return s.store.ListOverdue(ctx, overdueClass, from, to, offset, limit)
}

// Profile 查借据档案（跨库联邦）。
func (s *LoanService) Profile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	return s.store.GetProfile(ctx, loanNo)
}
```

- [ ] **Step 2: 写 router.go**

`templates/bank/internal/loan/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 loan 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/loan/products", h.ListProducts)
		r.Get("/loan/accounts", h.ListAccounts)
		r.Get("/loan/accounts/{loan_no}", h.GetAccount)
		r.Get("/loan/accounts/{loan_no}/profile", h.GetProfile)
		r.Get("/loan/balances", h.ListBalances)
		r.Get("/loan/overdue", h.ListOverdue)
	})
	return r
}
```

- [ ] **Step 3: 写 handlers.go**

`templates/bank/internal/loan/api/handlers.go`:
```go
// Package api 是 loan 服务的传输层：http handlers + chi router。
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/loan/domain"
	"bank/internal/loan/service"

	"github.com/go-chi/chi/v5"
)

// Handlers 持有 loan 只读服务。生产由 Svc 代理 repo；单测用
// service.NewLoanService(fakeLoanRepo) 注入。
type Handlers struct {
	Svc *service.LoanService
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListProducts 列贷款产品。
func (h *Handlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListProducts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanProductResp, 0, len(list))
	for _, p := range list {
		out = append(out, loanProductResp{
			ProductCode: p.ProductCode, ProductName: p.ProductName, LoanType: p.LoanType,
			MinRate: p.MinRate, MaxRate: p.MaxRate, MaxTerm: p.MaxTerm,
			MaxAmount: p.MaxAmount.String(), Status: p.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

// ListAccounts 按产品/状态筛选借据（query: product_code/status/offset/limit）。
func (h *Handlers) ListAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListAccounts(r.Context(), q.Get("product_code"), q.Get("status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanAccountResp, 0, len(list))
	for _, a := range list {
		out = append(out, accountRespOf(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// GetAccount 查单个借据。不存在返回 404。
func (h *Handlers) GetAccount(w http.ResponseWriter, r *http.Request) {
	no := chi.URLParam(r, "loan_no")
	a, err := h.Svc.GetAccount(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("借据不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, accountRespOf(a))
}

// ListBalances 按日期范围查逐日余额快照（query: from/to/loan_no/offset/limit）。
func (h *Handlers) ListBalances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListBalances(r.Context(), q.Get("from"), q.Get("to"), q.Get("loan_no"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanBalanceResp, 0, len(list))
	for _, b := range list {
		out = append(out, loanBalanceResp{
			LoanNo: b.LoanNo, BizDate: b.BizDate,
			PrincipalBalance: b.PrincipalBalance.String(), InterestReceivable: b.InterestReceivable.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": out})
}

// ListOverdue 按五级分类/日期范围查逾期（query: overdue_class/from/to/offset/limit）。
func (h *Handlers) ListOverdue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListOverdue(r.Context(), q.Get("overdue_class"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanOverdueResp, 0, len(list))
	for _, o := range list {
		out = append(out, loanOverdueResp{
			OverdueID: o.OverdueID, BizDate: o.BizDate, LoanNo: o.LoanNo,
			OverdueDays: o.OverdueDays, OverdueClass: o.OverdueClass, OverdueAmount: o.OverdueAmount.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"overdues": out})
}

// GetProfile 查借据档案（跨库联邦 JOIN）。
func (h *Handlers) GetProfile(w http.ResponseWriter, r *http.Request) {
	no := chi.URLParam(r, "loan_no")
	p, err := h.Svc.Profile(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("借据不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, loanProfileResp{
		LoanNo: p.LoanNo, CustID: p.CustID, Principal: p.Principal.String(),
		Balance: p.Balance.String(), Rate: p.Rate, Status: p.Status,
		CustName: p.CustName, CustType: p.CustType,
	})
}

// accountRespOf 借据 → DTO。
func accountRespOf(a domain.LoanAccount) loanAccountResp {
	return loanAccountResp{
		LoanNo: a.LoanNo, CustID: a.CustID, ProductCode: a.ProductCode, Ccy: a.Ccy,
		Principal: a.Principal.String(), Balance: a.Balance.String(), Rate: a.Rate,
		StartBizDate: a.StartBizDate, MatureDate: a.MatureDate, TermMonths: a.TermMonths,
		Status: a.Status, GuaranteeType: a.GuaranteeType, BranchCode: a.BranchCode,
	}
}

// --- DTO ---

type loanProductResp struct {
	ProductCode string `json:"product_code"`
	ProductName string `json:"product_name"`
	LoanType    string `json:"loan_type,omitempty"`
	MinRate     string `json:"min_rate,omitempty"`
	MaxRate     string `json:"max_rate,omitempty"`
	MaxTerm     int    `json:"max_term,omitempty"`
	MaxAmount   string `json:"max_amount"`
	Status      string `json:"status,omitempty"`
}

type loanAccountResp struct {
	LoanNo        string `json:"loan_no"`
	CustID        string `json:"cust_id"`
	ProductCode   string `json:"product_code"`
	Ccy           string `json:"ccy,omitempty"`
	Principal     string `json:"principal"`
	Balance       string `json:"balance"`
	Rate          string `json:"rate"`
	StartBizDate  string `json:"start_biz_date,omitempty"`
	MatureDate    string `json:"mature_date,omitempty"`
	TermMonths    int    `json:"term_months,omitempty"`
	Status        string `json:"status,omitempty"`
	GuaranteeType string `json:"guarantee_type,omitempty"`
	BranchCode    string `json:"branch_code,omitempty"`
}

type loanBalanceResp struct {
	LoanNo             string `json:"loan_no"`
	BizDate            string `json:"biz_date"`
	PrincipalBalance   string `json:"principal_balance"`
	InterestReceivable string `json:"interest_receivable"`
}

type loanOverdueResp struct {
	OverdueID     string `json:"overdue_id"`
	BizDate       string `json:"biz_date"`
	LoanNo        string `json:"loan_no"`
	OverdueDays   int    `json:"overdue_days"`
	OverdueClass  string `json:"overdue_class"`
	OverdueAmount string `json:"overdue_amount"`
}

type loanProfileResp struct {
	LoanNo    string `json:"loan_no"`
	CustID    string `json:"cust_id"`
	Principal string `json:"principal"`
	Balance   string `json:"balance"`
	Rate      string `json:"rate"`
	Status    string `json:"status,omitempty"`
	CustName  string `json:"cust_name,omitempty"`
	CustType  string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
```

- [ ] **Step 4: 写 handlers_test.go**

`templates/bank/internal/loan/api/handlers_test.go`:
```go
package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/loan/domain"
	"bank/internal/loan/service"
)

type fakeLoanRepo struct {
	account  *domain.LoanAccount
	profile  *domain.LoanProfile
	products []domain.LoanProduct
	// 记录最近一次 ListAccounts 参数
	gotProductCode string
	gotStatus      string
	gotOffset      int
	gotLimit       int
}

func (f *fakeLoanRepo) ListProducts(context.Context) ([]domain.LoanProduct, error) {
	return f.products, nil
}

func (f *fakeLoanRepo) ListAccounts(_ context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	f.gotProductCode, f.gotStatus, f.gotOffset, f.gotLimit = productCode, status, offset, limit
	if f.account != nil {
		return []domain.LoanAccount{*f.account}, nil
	}
	return nil, nil
}

func (f *fakeLoanRepo) GetAccount(_ context.Context, loanNo string) (domain.LoanAccount, error) {
	if f.account != nil && f.account.LoanNo == loanNo {
		return *f.account, nil
	}
	return domain.LoanAccount{}, sql.ErrNoRows
}

func (f *fakeLoanRepo) ListBalances(context.Context, string, string, string, int, int) ([]domain.LoanBalance, error) {
	return nil, nil
}

func (f *fakeLoanRepo) ListOverdue(context.Context, string, string, string, int, int) ([]domain.LoanOverdue, error) {
	return nil, nil
}

func (f *fakeLoanRepo) GetProfile(_ context.Context, loanNo string) (domain.LoanProfile, error) {
	if f.profile != nil && f.profile.LoanNo == loanNo {
		return *f.profile, nil
	}
	return domain.LoanProfile{}, sql.ErrNoRows
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + path)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_OK(t *testing.T) {
	fake := &fakeLoanRepo{account: &domain.LoanAccount{
		LoanNo: "LN0000001", CustID: "C0000001", Principal: domain.NewMoneyFromCents(1000000), Balance: domain.NewMoneyFromCents(900000), Rate: "0.043500",
	}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts/LN0000001")
	if code != 200 || !strings.Contains(body, `"principal":"10000.00"`) || !strings.Contains(body, `"balance":"9000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewLoanService(&fakeLoanRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/loan/accounts/LN9999999")
	if code != 404 {
		t.Errorf("code=%d want 404", code)
	}
}

func TestListAccounts_FiltersAndPagination(t *testing.T) {
	fake := &fakeLoanRepo{account: &domain.LoanAccount{LoanNo: "LN0000001", CustID: "C1"}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts?product_code=LP-CONS&status=disbursed&offset=10&limit=5")
	if code != 200 || !strings.Contains(body, `"accounts"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
	if fake.gotProductCode != "LP-CONS" || fake.gotStatus != "disbursed" || fake.gotOffset != 10 || fake.gotLimit != 5 {
		t.Errorf("参数透传错: %+v", fake)
	}
}

func TestGetProfile(t *testing.T) {
	fake := &fakeLoanRepo{profile: &domain.LoanProfile{
		LoanNo: "LN0000001", CustID: "C0000001", Principal: domain.NewMoneyFromCents(1000000), Balance: domain.NewMoneyFromCents(900000), Rate: "0.043500", CustName: "张伟", CustType: "个人",
	}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts/LN0000001/profile")
	if code != 200 || !strings.Contains(body, "张伟") || !strings.Contains(body, `"cust_type":"个人"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
```

- [ ] **Step 5: 写 cmd/loan/main.go**

`templates/bank/cmd/loan/main.go`（逐字镜像 `cmd/reward/main.go`，只换名字/库/端口）:
```go
// Package main 是 loan 只读 API 服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank/internal/loan/api"
	"bank/internal/loan/repo"
	"bank/internal/loan/service"
	"bank/internal/platform/pg"
)

func main() {
	dbName := getenv("DB_NAME", "loan_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Svc: service.NewLoanService(repo.NewLoanRepo(db)),
	}
	port := getenv("API_PORT", "8085")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("loan 监听 :%s (db=%s)", port, dbName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

type pinger interface{ Ping() error }

func waitForDB(p pinger, retries int, wait time.Duration) error {
	var err error
	for i := 0; i < retries; i++ {
		if err = p.Ping(); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: 跑测试 + build**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/loan/... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
```
Expected: PASS + build 成功（含 `cmd/loan`）。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/loan/service/ templates/bank/internal/loan/api/ templates/bank/cmd/loan/
git commit -m "feat(bank): loan service+api+cmd 四层纵切 (B-4b)"
```

---

### Task 7: wealth domain（Money 副本 + 5 表模型 + WealthProfile）

**Files:**
- Create: `templates/bank/internal/wealth/domain/money.go`
- Create: `templates/bank/internal/wealth/domain/wealth.go`
- Test: `templates/bank/internal/wealth/domain/money_test.go`
- Test: `templates/bank/internal/wealth/domain/wealth_test.go`

**Interfaces:**
- Consumes: 无（最内层）。
- Produces（Task 8/9/10 依赖，逐字一致）: `Money` 全套（同 loan）；结构体 `WealthProduct`/`WealthNav`/`WealthHolding`/`WealthOrder`/`WealthIncome`/`WealthProfile`（字段见 Step 2）。

- [ ] **Step 1: 复制 Money**

```bash
cp templates/bank/internal/reward/domain/money.go templates/bank/internal/wealth/domain/money.go
cp templates/bank/internal/reward/domain/money_test.go templates/bank/internal/wealth/domain/money_test.go
```

- [ ] **Step 2: 写 wealth.go 领域模型**

`templates/bank/internal/wealth/domain/wealth.go`:
```go
// Package domain 是 wealth 服务的纯领域模型，零 DB/框架依赖（最内层）。
// 金额字段（cost/current_value/amount/min_amount）用 Money（int64 分）；
// nav/accum_nav/share/expected_return 是非货币小数，NUMERIC 文本直存（对齐 risk 的 risk_score 边界）。
package domain

// WealthProduct 对应 wealth_product 表。
type WealthProduct struct {
	ProductCode    string
	ProductName    string
	ProductType    string
	RiskLevel      string
	ExpectedReturn string // NUMERIC(10,6) 文本（比率，非金额）
	MinAmount      Money
	TermDays       int
	StartBizDate   string
	EndBizDate     string
	Status         string
}

// WealthNav 对应 wealth_nav 表（逐日全量净值快照）。
type WealthNav struct {
	ProductCode string
	BizDate     string
	Nav         string // NUMERIC(12,6) 文本（非金额）
	AccumNav    string // NUMERIC(12,6) 文本（非金额）
}

// WealthHolding 对应 wealth_holding 表。
type WealthHolding struct {
	HoldingID    string
	CustID       string
	AccountNo    string
	ProductCode  string
	Ccy          string
	Share        string // NUMERIC(18,4) 文本（非金额）
	Cost         Money
	CurrentValue Money
	BizDate      string
}

// WealthOrder 对应 wealth_order 表。
type WealthOrder struct {
	OrderID     string
	BizDate     string
	CustID      string
	ProductCode string
	AccountNo   string
	OrderType   string
	Amount      Money
	Share       string // NUMERIC(18,4) 文本（非金额）
	Nav         string // NUMERIC(12,6) 文本（非金额）
	Status      string
}

// WealthIncome 对应 wealth_income 表（B-4b Q1-B 每日利息）。
type WealthIncome struct {
	IncomeID   string
	BizDate    string
	HoldingID  string
	IncomeType string
	Amount     Money
}

// WealthProfile 是联邦查询结果（wealth_holding JOIN ext_cust_db_cust_info）。
type WealthProfile struct {
	HoldingID    string
	CustID       string
	ProductCode  string
	Share        string
	CurrentValue Money
	CustName     string
	CustType     string
}
```

- [ ] **Step 3: 写 wealth_test.go**

`templates/bank/internal/wealth/domain/wealth_test.go`:
```go
package domain

import "testing"

func TestWealthHolding(t *testing.T) {
	h := WealthHolding{HoldingID: "WP-HD-0000000", CustID: "C0000001", Share: "1050.2500", Cost: NewMoneyFromCents(100000)}
	if h.HoldingID != "WP-HD-0000000" || h.Cost.String() != "1000.00" {
		t.Errorf("got %+v", h)
	}
}

func TestWealthOrderMoneyRoundTrip(t *testing.T) {
	o := WealthOrder{OrderID: "WP-OD-20250601-00000", Amount: NewMoneyFromCents(500050), Nav: "1.023456"}
	if o.Amount.String() != "5000.50" {
		t.Errorf("order 金额错: %s", o.Amount)
	}
	p, err := ParseCents("5000.50")
	if err != nil || p != o.Amount {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}
```

- [ ] **Step 4: 跑测试**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/wealth/domain/...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/wealth/domain/
git commit -m "feat(bank): wealth domain——Money 副本 + 5 表模型 (B-4b)"
```

---

### Task 8: wealth fixture 生成器（静态 + 逐日 NAV 滚存 + 三因子订单 + 每日利息）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/wealth.go`
- Test: `templates/bank/internal/fixtures/domains/wealth_test.go`

**Interfaces:**
- Consumes: Task 2 的 `fixtures.WealthProducts`/`OrderTypes`/`IncomeTypes`；既有 helpers（同 Task 4）；Task 7 的 `bank/internal/wealth/domain`。
- Produces（Task 11 依赖，逐字一致）:
  - `type WealthStatic struct { Products []domain.WealthProduct; Holdings []domain.WealthHolding }`
  - `GenWealthStatic(cfg fixtures.Config, custIDs []string, demandAccounts []string) WealthStatic`
  - `WriteWealthStatic(ctx context.Context, db *sql.DB, s WealthStatic) error`
  - `RunWealth(ctx context.Context, db *sql.DB, cfg fixtures.Config, products []domain.WealthProduct, holdings []domain.WealthHolding, custIDs []string, demandAccounts []string) error`

- [ ] **Step 1: 写 wealth.go**

`templates/bank/internal/fixtures/domains/wealth.go`（rng `seed+50` 静态 / `seed+51+ordinal` 逐日；navState 跨日滚存——路径依赖，全量重放确定）:
```go
package domains

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/wealth/domain"
)

// WealthStatic 静态表行集合（一次性生成；持仓与产品共享 rng seed+50，避免与逐日 +51 碰撞）。
type WealthStatic struct {
	Products []domain.WealthProduct
	Holdings []domain.WealthHolding
}

// GenWealthStatic 生成理财产品 + 初始持仓（每客户 0-3 个）。
func GenWealthStatic(cfg fixtures.Config, custIDs []string, demandAccounts []string) WealthStatic {
	rng := fixtures.NewRNG(cfg.Seed + 50)
	products := make([]domain.WealthProduct, len(fixtures.WealthProducts))
	for i, p := range fixtures.WealthProducts {
		products[i] = domain.WealthProduct{
			ProductCode: p.Code, ProductName: p.Name, ProductType: p.Type, RiskLevel: p.Risk,
			ExpectedReturn: fmt.Sprintf("%.6f", p.ExpectedReturn),
			MinAmount:      domain.NewMoneyFromCents(int64(p.MinAmountYuan) * 100),
			TermDays:       p.TermDays,
			StartBizDate:   cfg.StartBizDate, EndBizDate: addDays(cfg.EndBizDate, 365),
			Status:         "active",
		}
	}
	var holdings []domain.WealthHolding
	idx := 0
	for _, cid := range custIDs {
		n := rng.IntRange(0, 3)
		for j := 0; j < n; j++ {
			p := fixtures.WealthProducts[rng.IntRange(0, len(fixtures.WealthProducts)-1)]
			nav0 := 1 + rng.Float64()*0.25 // 4dp；spec §6.4（原实现为 1+uniform(-0.05,0.2)，这里有意对齐为 [1,1.25)）
			amountYuan := maxInt(p.MinAmountYuan, rng.IntRange(0, 99999)*100)
			amount := domain.NewMoneyFromCents(int64(amountYuan) * 100)
			holdings = append(holdings, domain.WealthHolding{
				HoldingID: fmt.Sprintf("WP-HD-%07d", idx), CustID: cid,
				AccountNo: pickStr(rng, demandAccounts), ProductCode: p.Code, Ccy: "CNY",
				Share: fmt.Sprintf("%.4f", float64(amountYuan)/nav0), // 非金额小数，4dp 文本
				Cost:  amount, CurrentValue: amount,
				BizDate: cfg.StartBizDate,
			})
			idx++
		}
	}
	return WealthStatic{Products: products, Holdings: holdings}
}

// WriteWealthStatic 幂等写 wealth_product/wealth_holding（先 DELETE 后 INSERT）。
func WriteWealthStatic(ctx context.Context, db *sql.DB, s WealthStatic) error {
	for _, t := range []string{"wealth_holding", "wealth_product"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range s.Products {
		if _, err := db.ExecContext(ctx, `INSERT INTO wealth_product(product_code,product_name,product_type,risk_level,expected_return,min_amount,term_days,start_biz_date,end_biz_date,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			p.ProductCode, p.ProductName, p.ProductType, p.RiskLevel, p.ExpectedReturn,
			p.MinAmount.String(), p.TermDays, p.StartBizDate, p.EndBizDate, p.Status); err != nil {
			return err
		}
	}
	for _, h := range s.Holdings {
		if _, err := db.ExecContext(ctx, `INSERT INTO wealth_holding(holding_id,cust_id,account_no,product_code,ccy,share,cost,current_value,biz_date)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			h.HoldingID, h.CustID, h.AccountNo, h.ProductCode, h.Ccy, h.Share,
			h.Cost.String(), h.CurrentValue.String(), h.BizDate); err != nil {
			return err
		}
	}
	return nil
}

// RunWealth 按 bizDate 推进：每日 NAV 游走（滚存）+ 三因子订单（每日独立 rng）+ 每日利息（Q1-B）。
// 每业务日一个 pg.RunInTx。
func RunWealth(ctx context.Context, db *sql.DB, cfg fixtures.Config, products []domain.WealthProduct, holdings []domain.WealthHolding, custIDs []string, demandAccounts []string) error {
	if len(products) == 0 {
		return fmt.Errorf("wealth: 无产品")
	}
	if len(custIDs) == 0 || len(demandAccounts) == 0 {
		return fmt.Errorf("wealth: 无客户或活期账户")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("wealth: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	navState := make(map[string]float64, len(products))
	prodRet := make(map[string]float64, len(products))
	for _, p := range products {
		ret, _ := strconv.ParseFloat(p.ExpectedReturn, 64)
		prodRet[p.ProductCode] = ret
		navState[p.ProductCode] = 1 + ret/365 // 每日按预期年化微涨
	}
	// 持仓成本快照（供 income；订单不改持仓）
	type holdingCost struct {
		costCents int64
		prodCode  string
	}
	costs := make([]holdingCost, len(holdings))
	for i, h := range holdings {
		costs[i] = holdingCost{costCents: h.Cost.Cents(), prodCode: h.ProductCode}
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		rng := fixtures.NewRNG(cfg.Seed + 51 + dayOrdinal(d, base)) // per-day rng（对齐 reward）
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		// NAV 游走（路径依赖：navState 跨日滚存）
		navRows := make([]domain.WealthNav, 0, len(products))
		for _, p := range products {
			drift := navState[p.ProductCode] * (1 + (rng.Float64()*0.006 - 0.002))
			navState[p.ProductCode] = math.Round(math.Max(0.5, drift)*1e6) / 1e6
			navRows = append(navRows, domain.WealthNav{
				ProductCode: p.ProductCode, BizDate: dateStr,
				Nav:      fmt.Sprintf("%.6f", navState[p.ProductCode]),
				AccumNav: fmt.Sprintf("%.6f", navState[p.ProductCode]*1.1),
			})
		}
		// 三因子订单
		n := orderVolumeForDay(sf, factor)
		orders := make([]domain.WealthOrder, 0, n)
		for i := 0; i < n; i++ {
			p := fixtures.WealthProducts[rng.IntRange(0, len(fixtures.WealthProducts)-1)]
			amountYuan := maxInt(p.MinAmountYuan, rng.IntRange(0, 99999)*100) // 同持仓公式
			orders = append(orders, domain.WealthOrder{
				OrderID: fmt.Sprintf("WP-OD-%s-%05d", compact, i),
				BizDate: dateStr, CustID: pickStr(rng, custIDs), ProductCode: p.Code,
				AccountNo: pickStr(rng, demandAccounts), OrderType: rng.Choice(fixtures.OrderTypes),
				Amount: domain.NewMoneyFromCents(int64(amountYuan) * 100),
				Share:  fmt.Sprintf("%.4f", float64(rng.IntRange(0, 999))), // 有意保留的怪癖：share 独立随机，不由 amount/nav 推导
				Nav:    fmt.Sprintf("%.6f", navState[p.Code]),
				Status: "done",
			})
		}
		// 每日利息（Q1-B）：每持仓 cost × expected_return / 365，四舍五入到分
		incomes := make([]domain.WealthIncome, 0, len(costs))
		for i, hc := range costs {
			incomes = append(incomes, domain.WealthIncome{
				IncomeID: fmt.Sprintf("WP-IC-%s-%05d", compact, i),
				BizDate:  dateStr, HoldingID: holdings[i].HoldingID,
				IncomeType: fixtures.IncomeTypes[0],
				Amount:     domain.NewMoneyFromCents(int64(math.Round(float64(hc.costCents) * prodRet[hc.prodCode] / 365))),
			})
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_nav WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_nav %s: %w", dateStr, err)
			}
			if err := bulkInsertWealthNavs(ctx, q, navRows); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_order WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_order %s: %w", dateStr, err)
			}
			if err := bulkInsertWealthOrders(ctx, q, orders); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_income WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_income %s: %w", dateStr, err)
			}
			return bulkInsertWealthIncomes(ctx, q, incomes)
		}); err != nil {
			return fmt.Errorf("wealth: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// orderVolumeForDay 当日理财订单笔数（三因子缩放；提取为函数供单测比周末/工作日）。
func orderVolumeForDay(sf, factor float64) int {
	return maxInt(0, int(20*sf*factor))
}

// bulkInsertWealthNavs 批量插 wealth_nav（4 列）。
func bulkInsertWealthNavs(ctx context.Context, q pg.DBTX, rows []domain.WealthNav) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 4
	const stmt = "INSERT INTO wealth_nav(product_code,biz_date,nav,accum_nav) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.ProductCode, r.BizDate, r.Nav, r.AccumNav)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_nav: %w", err)
		}
	}
	return nil
}

// bulkInsertWealthOrders 批量插 wealth_order（10 列）。
func bulkInsertWealthOrders(ctx context.Context, q pg.DBTX, rows []domain.WealthOrder) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 10
	const stmt = "INSERT INTO wealth_order(order_id,biz_date,cust_id,product_code,account_no,order_type,amount,share,nav,status) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, o := range chunk {
			args = append(args, o.OrderID, o.BizDate, o.CustID, o.ProductCode, o.AccountNo,
				o.OrderType, o.Amount.String(), o.Share, o.Nav, o.Status)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_order: %w", err)
		}
	}
	return nil
}

// bulkInsertWealthIncomes 批量插 wealth_income（5 列）。
func bulkInsertWealthIncomes(ctx context.Context, q pg.DBTX, rows []domain.WealthIncome) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 5
	const stmt = "INSERT INTO wealth_income(income_id,biz_date,holding_id,income_type,amount) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.IncomeID, r.BizDate, r.HoldingID, r.IncomeType, r.Amount.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_income: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 2: 写 wealth_test.go（确定性 + 周末<工作日）**

`templates/bank/internal/fixtures/domains/wealth_test.go`:
```go
package domains

import (
	"fmt"
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenWealthStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003", "C0000004"}
	demandNos := []string{"D0000000001", "D0000000002"}
	a := GenWealthStatic(cfg, custIDs, demandNos)
	b := GenWealthStatic(cfg, custIDs, demandNos)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenWealthStatic 不确定")
	}
	if len(a.Products) != 6 {
		t.Errorf("产品应 6 个, got %d", len(a.Products))
	}
	p := a.Products[0]
	if p.ProductCode != "WP-FIX1" || p.ExpectedReturn != "0.035000" {
		t.Errorf("产品元组错: %+v", p)
	}
	if p.EndBizDate != addDays(cfg.EndBizDate, 365) {
		t.Errorf("产品 end=%s 应=end+365d", p.EndBizDate)
	}
	for i, h := range a.Holdings {
		if want := fmt.Sprintf("WP-HD-%07d", i); h.HoldingID != want {
			t.Errorf("holding_id=%s want %s", h.HoldingID, want)
		}
		if h.Cost.Cents() <= 0 || h.CurrentValue != h.Cost {
			t.Errorf("持仓成本错: %+v", h)
		}
		if h.BizDate != cfg.StartBizDate {
			t.Errorf("持仓 biz_date=%s 应=start", h.BizDate)
		}
	}
}

func TestWealthOrderVolume_WeekendLower(t *testing.T) {
	sat := parseDate("2025-06-07") // 周六
	mon := parseDate("2025-06-09") // 周一
	if sat.Weekday().String() != "Saturday" || mon.Weekday().String() != "Monday" {
		t.Fatalf("测试日期星期错: %s %s", sat.Weekday(), mon.Weekday())
	}
	sf := fixtures.ScaleFactor(fixtures.ScaleDev)
	fSat := trendFactor(sat) * seasonalFactor(sat) * cyclicalFactor(sat)
	fMon := trendFactor(mon) * seasonalFactor(mon) * cyclicalFactor(mon)
	nSat, nMon := orderVolumeForDay(sf, fSat), orderVolumeForDay(sf, fMon)
	if nSat >= nMon {
		t.Errorf("周末订单量(%d) 应 < 工作日(%d)", nSat, nMon)
	}
}
```

- [ ] **Step 3: 跑测试 + build**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
```
Expected: PASS + build 成功。

- [ ] **Step 4: Commit**
```bash
git add templates/bank/internal/fixtures/domains/wealth.go templates/bank/internal/fixtures/domains/wealth_test.go
git commit -m "feat(bank): wealth fixture 生成器——NAV 滚存 + 三因子订单 + 每日利息 (B-4b)"
```

---

### Task 9: wealth repo（本库查询 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/wealth/repo/wealth_repo.go`
- Test: `templates/bank/internal/wealth/repo/wealth_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: Task 7 domain；Task 1 表结构；Task 2 FDW 外部表。
- Produces（Task 10 依赖，逐字一致）:
  - `type WealthRepo struct{ db *sql.DB }` + `NewWealthRepo(db *sql.DB) *WealthRepo`
  - `ListProducts(ctx context.Context) ([]domain.WealthProduct, error)`
  - `ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error)`（不分页，spec §9 无 offset/limit）
  - `ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error)`
  - `ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error)`
  - `ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error)`
  - `GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error)`

- [ ] **Step 1: 写 wealth_repo.go**

`templates/bank/internal/wealth/repo/wealth_repo.go`:
```go
// Package repo 是 wealth 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/wealth/domain"
)

// WealthRepo wealth 仓储。本库 wealth_* 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type WealthRepo struct{ db *sql.DB }

// NewWealthRepo 构造 WealthRepo。
func NewWealthRepo(db *sql.DB) *WealthRepo { return &WealthRepo{db: db} }

// ListProducts 列理财产品（静态全量）。
func (r *WealthRepo) ListProducts(ctx context.Context) ([]domain.WealthProduct, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,product_name,product_type,risk_level,expected_return,min_amount,term_days,start_biz_date,end_biz_date,status
		FROM wealth_product ORDER BY product_code`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财产品: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthProduct
	for rows.Next() {
		var p domain.WealthProduct
		var risk, ret, minAmt, start, end, status sql.NullString
		if err := rows.Scan(&p.ProductCode, &p.ProductName, &p.ProductType, &risk, &ret, &minAmt, &p.TermDays, &start, &end, &status); err != nil {
			return nil, fmt.Errorf("repo: 列理财产品 scan: %w", err)
		}
		p.RiskLevel, p.ExpectedReturn, p.StartBizDate, p.EndBizDate, p.Status = risk.String, ret.String, start.String, end.String, status.String
		m, err := domain.ParseCents(minAmt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析起购金额: %w", err)
		}
		p.MinAmount = m
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财产品: %w", err)
	}
	return out, nil
}

// ListNav 按产品/日期范围查每日净值（空则不限；序列量小不分页）。
func (r *WealthRepo) ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,biz_date,nav,accum_nav FROM wealth_nav
		WHERE ($1='' OR product_code=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date, product_code`, productCode, from, to)
	if err != nil {
		return nil, fmt.Errorf("repo: 列净值: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthNav
	for rows.Next() {
		var n domain.WealthNav
		var nav, accum sql.NullString
		if err := rows.Scan(&n.ProductCode, &n.BizDate, &nav, &accum); err != nil {
			return nil, fmt.Errorf("repo: 列净值 scan: %w", err)
		}
		n.Nav, n.AccumNav = nav.String, accum.String
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列净值: %w", err)
	}
	return out, nil
}

// ListHoldings 按客户筛选持仓（空则不限），分页。limit<=0 取 50。
func (r *WealthRepo) ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT holding_id,cust_id,account_no,product_code,ccy,share,cost,current_value,biz_date
		FROM wealth_holding WHERE ($1='' OR cust_id=$1) ORDER BY holding_id LIMIT $2 OFFSET $3`, custID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列持仓: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthHolding
	for rows.Next() {
		h, err := scanHolding(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列持仓 scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列持仓: %w", err)
	}
	return out, nil
}

// ListOrders 按客户/产品/日期范围查订单（空则不限），分页。
func (r *WealthRepo) ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT order_id,biz_date,cust_id,product_code,account_no,order_type,amount,share,nav,status
		FROM wealth_order WHERE ($1='' OR cust_id=$1) AND ($2='' OR product_code=$2)
		AND (NULLIF($3,'') IS NULL OR biz_date >= NULLIF($3,'')::date)
		AND (NULLIF($4,'') IS NULL OR biz_date <= NULLIF($4,'')::date)
		ORDER BY biz_date DESC, order_id LIMIT $5 OFFSET $6`, custID, productCode, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财订单: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthOrder
	for rows.Next() {
		var o domain.WealthOrder
		var amt, share, nav, status sql.NullString
		if err := rows.Scan(&o.OrderID, &o.BizDate, &o.CustID, &o.ProductCode, &o.AccountNo, &o.OrderType, &amt, &share, &nav, &status); err != nil {
			return nil, fmt.Errorf("repo: 列理财订单 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析订单金额: %w", err)
		}
		o.Amount, o.Share, o.Nav, o.Status = m, share.String, nav.String, status.String
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财订单: %w", err)
	}
	return out, nil
}

// ListIncomes 按持仓/日期范围查收益（空则不限），分页。
func (r *WealthRepo) ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT income_id,biz_date,holding_id,income_type,amount FROM wealth_income
		WHERE ($1='' OR holding_id=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date DESC, income_id LIMIT $4 OFFSET $5`, holdingID, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财收益: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthIncome
	for rows.Next() {
		var inc domain.WealthIncome
		var itype, amt sql.NullString
		if err := rows.Scan(&inc.IncomeID, &inc.BizDate, &inc.HoldingID, &itype, &amt); err != nil {
			return nil, fmt.Errorf("repo: 列理财收益 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析收益金额: %w", err)
		}
		inc.IncomeType, inc.Amount = itype.String, m
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财收益: %w", err)
	}
	return out, nil
}

// GetHoldingProfile 跨库联邦：wealth_holding JOIN ext_cust_db_cust_info → 持仓份额/市值 + 客户姓名/类型。
func (r *WealthRepo) GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	var p domain.WealthProfile
	var share, cv, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT h.holding_id, h.cust_id, h.product_code, h.share, h.current_value, ci.name, ci.cust_type
		FROM wealth_holding h
		LEFT JOIN ext_cust_db_cust_info ci ON h.cust_id=ci.cust_id
		WHERE h.holding_id=$1`, holdingID).
		Scan(&p.HoldingID, &p.CustID, &p.ProductCode, &share, &cv, &name, &ctype)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 联邦查持仓档案 %s: %w", holdingID, err)
	}
	m, err := domain.ParseCents(cv.String)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 解析持仓市值: %w", err)
	}
	p.Share, p.CurrentValue, p.CustName, p.CustType = share.String, m, name.String, ctype.String
	return p, nil
}

// scanHolding 扫描单行 wealth_holding（scan 函数由 QueryRow 或 Rows 注入）。
func scanHolding(scan func(dest ...any) error) (domain.WealthHolding, error) {
	var h domain.WealthHolding
	var share, cost, cv sql.NullString
	if err := scan(&h.HoldingID, &h.CustID, &h.AccountNo, &h.ProductCode, &h.Ccy, &share, &cost, &cv, &h.BizDate); err != nil {
		return domain.WealthHolding{}, err
	}
	var err error
	if h.Cost, err = domain.ParseCents(cost.String); err != nil {
		return domain.WealthHolding{}, fmt.Errorf("解析持仓成本: %w", err)
	}
	if h.CurrentValue, err = domain.ParseCents(cv.String); err != nil {
		return domain.WealthHolding{}, fmt.Errorf("解析持仓市值: %w", err)
	}
	h.Share = share.String
	return h, nil
}
```

- [ ] **Step 2: 写 wealth_repo_test.go（integration）**

`templates/bank/internal/wealth/repo/wealth_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/wealth/repo"
)

func setupWealthDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("wealth_db")
	if err != nil {
		t.Skipf("无 wealth_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestWealthRepo_ListProducts(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	prods, err := repo.NewWealthRepo(db).ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != 6 {
		t.Errorf("理财产品应 6 个, got %d", len(prods))
	}
}

func TestWealthRepo_Lists(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewWealthRepo(db)
	if _, err := r.ListNav(ctx, "", "", ""); err != nil {
		t.Fatalf("ListNav 失败: %v", err)
	}
	if _, err := r.ListHoldings(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListHoldings 失败: %v", err)
	}
	if _, err := r.ListOrders(ctx, "", "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOrders 失败: %v", err)
	}
	if _, err := r.ListIncomes(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListIncomes 失败: %v", err)
	}
}

func TestWealthRepo_GetHoldingProfile_FDWJoin(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err := repo.NewWealthRepo(db).GetHoldingProfile(context.Background(), "WP-HD-NOPE")
	if err == nil {
		t.Error("不存在的持仓应返回错误")
	}
}
```

- [ ] **Step 3: build + vet**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go vet ./internal/wealth/...
```
Expected: 成功。

- [ ] **Step 4: Commit**
```bash
git add templates/bank/internal/wealth/repo/
git commit -m "feat(bank): wealth repo——本库查询 + FDW 联邦 JOIN (B-4b)"
```

---

### Task 10: wealth service + api + cmd

**Files:**
- Create: `templates/bank/internal/wealth/service/wealth_service.go`
- Create: `templates/bank/internal/wealth/api/router.go`
- Create: `templates/bank/internal/wealth/api/handlers.go`
- Test: `templates/bank/internal/wealth/api/handlers_test.go`
- Create: `templates/bank/cmd/wealth/main.go`

**Interfaces:**
- Consumes: Task 7 domain、Task 9 repo（`NewWealthRepo` + 6 个方法签名）。
- Produces: `service.WealthStore` 接口（= Task 9 的 6 个方法签名逐字）+ `NewWealthService(store WealthStore) *WealthService`；api `Handlers{Svc *service.WealthService}` + `NewRouter(h *Handlers) http.Handler`；`cmd/wealth` 可构建入口（compose `CMD: wealth`，Task 12）。
- 端点（spec §9）: `GET /healthz`、`GET /api/v1/wealth/products`、`GET /api/v1/wealth/nav`、`GET /api/v1/wealth/holdings`、`GET /api/v1/wealth/orders`、`GET /api/v1/wealth/incomes`、`GET /api/v1/wealth/holdings/{holding_id}/profile`。

- [ ] **Step 1: 写 wealth_service.go（薄封装）**

`templates/bank/internal/wealth/service/wealth_service.go`:
```go
// Package service 是 wealth 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/wealth/domain"
)

// WealthStore wealth 查询接口（repo 实现）。
type WealthStore interface {
	ListProducts(ctx context.Context) ([]domain.WealthProduct, error)
	ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error)
	ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error)
	ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error)
	ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error)
	GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error)
}

// WealthService wealth 只读服务，包装 WealthStore 做查询编排。
type WealthService struct{ store WealthStore }

// NewWealthService 构造 WealthService。
func NewWealthService(store WealthStore) *WealthService { return &WealthService{store: store} }

// ListProducts 列理财产品。
func (s *WealthService) ListProducts(ctx context.Context) ([]domain.WealthProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListNav 按产品/日期范围查每日净值。
func (s *WealthService) ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error) {
	return s.store.ListNav(ctx, productCode, from, to)
}

// ListHoldings 按客户筛选持仓并分页。
func (s *WealthService) ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	return s.store.ListHoldings(ctx, custID, offset, limit)
}

// ListOrders 按客户/产品/日期范围查订单。
func (s *WealthService) ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error) {
	return s.store.ListOrders(ctx, custID, productCode, from, to, offset, limit)
}

// ListIncomes 按持仓/日期范围查收益。
func (s *WealthService) ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error) {
	return s.store.ListIncomes(ctx, holdingID, from, to, offset, limit)
}

// HoldingProfile 查持仓档案（跨库联邦）。
func (s *WealthService) HoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	return s.store.GetHoldingProfile(ctx, holdingID)
}
```

- [ ] **Step 2: 写 router.go**

`templates/bank/internal/wealth/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 wealth 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/wealth/products", h.ListProducts)
		r.Get("/wealth/nav", h.ListNav)
		r.Get("/wealth/holdings", h.ListHoldings)
		r.Get("/wealth/holdings/{holding_id}/profile", h.GetHoldingProfile)
		r.Get("/wealth/orders", h.ListOrders)
		r.Get("/wealth/incomes", h.ListIncomes)
	})
	return r
}
```

- [ ] **Step 3: 写 handlers.go**

`templates/bank/internal/wealth/api/handlers.go`:
```go
// Package api 是 wealth 服务的传输层：http handlers + chi router。
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/wealth/domain"
	"bank/internal/wealth/service"

	"github.com/go-chi/chi/v5"
)

// Handlers 持有 wealth 只读服务。生产由 Svc 代理 repo；单测用
// service.NewWealthService(fakeWealthRepo) 注入。
type Handlers struct {
	Svc *service.WealthService
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListProducts 列理财产品。
func (h *Handlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListProducts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthProductResp, 0, len(list))
	for _, p := range list {
		out = append(out, wealthProductResp{
			ProductCode: p.ProductCode, ProductName: p.ProductName, ProductType: p.ProductType,
			RiskLevel: p.RiskLevel, ExpectedReturn: p.ExpectedReturn, MinAmount: p.MinAmount.String(),
			TermDays: p.TermDays, StartBizDate: p.StartBizDate, EndBizDate: p.EndBizDate, Status: p.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

// ListNav 按产品/日期范围查每日净值（query: product_code/from/to）。
func (h *Handlers) ListNav(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.Svc.ListNav(r.Context(), q.Get("product_code"), q.Get("from"), q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthNavResp, 0, len(list))
	for _, n := range list {
		out = append(out, wealthNavResp{
			ProductCode: n.ProductCode, BizDate: n.BizDate, Nav: n.Nav, AccumNav: n.AccumNav,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"navs": out})
}

// ListHoldings 按客户筛选持仓（query: cust_id/offset/limit）。
func (h *Handlers) ListHoldings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListHoldings(r.Context(), q.Get("cust_id"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthHoldingResp, 0, len(list))
	for _, hd := range list {
		out = append(out, holdingRespOf(hd))
	}
	writeJSON(w, http.StatusOK, map[string]any{"holdings": out})
}

// ListOrders 按客户/产品/日期范围查订单（query: cust_id/product_code/from/to/offset/limit）。
func (h *Handlers) ListOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListOrders(r.Context(), q.Get("cust_id"), q.Get("product_code"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthOrderResp, 0, len(list))
	for _, o := range list {
		out = append(out, wealthOrderResp{
			OrderID: o.OrderID, BizDate: o.BizDate, CustID: o.CustID, ProductCode: o.ProductCode,
			AccountNo: o.AccountNo, OrderType: o.OrderType, Amount: o.Amount.String(),
			Share: o.Share, Nav: o.Nav, Status: o.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// ListIncomes 按持仓/日期范围查收益（query: holding_id/from/to/offset/limit）。
func (h *Handlers) ListIncomes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListIncomes(r.Context(), q.Get("holding_id"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthIncomeResp, 0, len(list))
	for _, inc := range list {
		out = append(out, wealthIncomeResp{
			IncomeID: inc.IncomeID, BizDate: inc.BizDate, HoldingID: inc.HoldingID,
			IncomeType: inc.IncomeType, Amount: inc.Amount.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"incomes": out})
}

// GetHoldingProfile 查持仓档案（跨库联邦 JOIN）。不存在返回 404。
func (h *Handlers) GetHoldingProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "holding_id")
	p, err := h.Svc.HoldingProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("持仓不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, wealthProfileResp{
		HoldingID: p.HoldingID, CustID: p.CustID, ProductCode: p.ProductCode,
		Share: p.Share, CurrentValue: p.CurrentValue.String(),
		CustName: p.CustName, CustType: p.CustType,
	})
}

// holdingRespOf 持仓 → DTO。
func holdingRespOf(hd domain.WealthHolding) wealthHoldingResp {
	return wealthHoldingResp{
		HoldingID: hd.HoldingID, CustID: hd.CustID, AccountNo: hd.AccountNo, ProductCode: hd.ProductCode,
		Ccy: hd.Ccy, Share: hd.Share, Cost: hd.Cost.String(), CurrentValue: hd.CurrentValue.String(),
		BizDate: hd.BizDate,
	}
}

// --- DTO ---

type wealthProductResp struct {
	ProductCode    string `json:"product_code"`
	ProductName    string `json:"product_name"`
	ProductType    string `json:"product_type,omitempty"`
	RiskLevel      string `json:"risk_level,omitempty"`
	ExpectedReturn string `json:"expected_return,omitempty"`
	MinAmount      string `json:"min_amount"`
	TermDays       int    `json:"term_days,omitempty"`
	StartBizDate   string `json:"start_biz_date,omitempty"`
	EndBizDate     string `json:"end_biz_date,omitempty"`
	Status         string `json:"status,omitempty"`
}

type wealthNavResp struct {
	ProductCode string `json:"product_code"`
	BizDate     string `json:"biz_date"`
	Nav         string `json:"nav"`
	AccumNav    string `json:"accum_nav"`
}

type wealthHoldingResp struct {
	HoldingID    string `json:"holding_id"`
	CustID       string `json:"cust_id"`
	AccountNo    string `json:"account_no,omitempty"`
	ProductCode  string `json:"product_code"`
	Ccy          string `json:"ccy,omitempty"`
	Share        string `json:"share"`
	Cost         string `json:"cost"`
	CurrentValue string `json:"current_value"`
	BizDate      string `json:"biz_date,omitempty"`
}

type wealthOrderResp struct {
	OrderID     string `json:"order_id"`
	BizDate     string `json:"biz_date"`
	CustID      string `json:"cust_id"`
	ProductCode string `json:"product_code"`
	AccountNo   string `json:"account_no,omitempty"`
	OrderType   string `json:"order_type"`
	Amount      string `json:"amount"`
	Share       string `json:"share,omitempty"`
	Nav         string `json:"nav,omitempty"`
	Status      string `json:"status,omitempty"`
}

type wealthIncomeResp struct {
	IncomeID   string `json:"income_id"`
	BizDate    string `json:"biz_date"`
	HoldingID  string `json:"holding_id"`
	IncomeType string `json:"income_type,omitempty"`
	Amount     string `json:"amount"`
}

type wealthProfileResp struct {
	HoldingID    string `json:"holding_id"`
	CustID       string `json:"cust_id"`
	ProductCode  string `json:"product_code"`
	Share        string `json:"share"`
	CurrentValue string `json:"current_value"`
	CustName     string `json:"cust_name,omitempty"`
	CustType     string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
```

- [ ] **Step 4: 写 handlers_test.go**

`templates/bank/internal/wealth/api/handlers_test.go`:
```go
package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/wealth/domain"
	"bank/internal/wealth/service"
)

type fakeWealthRepo struct {
	holding *domain.WealthHolding
	profile *domain.WealthProfile
	// 记录最近一次 ListHoldings 参数
	gotCustID string
	gotOffset int
	gotLimit  int
}

func (f *fakeWealthRepo) ListProducts(context.Context) ([]domain.WealthProduct, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListNav(context.Context, string, string, string) ([]domain.WealthNav, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListHoldings(_ context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	f.gotCustID, f.gotOffset, f.gotLimit = custID, offset, limit
	if f.holding != nil {
		return []domain.WealthHolding{*f.holding}, nil
	}
	return nil, nil
}

func (f *fakeWealthRepo) ListOrders(context.Context, string, string, string, string, int, int) ([]domain.WealthOrder, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListIncomes(context.Context, string, string, string, int, int) ([]domain.WealthIncome, error) {
	return nil, nil
}

func (f *fakeWealthRepo) GetHoldingProfile(_ context.Context, holdingID string) (domain.WealthProfile, error) {
	if f.profile != nil && f.profile.HoldingID == holdingID {
		return *f.profile, nil
	}
	return domain.WealthProfile{}, sql.ErrNoRows
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + path)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestListHoldings_FiltersAndPagination(t *testing.T) {
	fake := &fakeWealthRepo{holding: &domain.WealthHolding{
		HoldingID: "WP-HD-0000001", CustID: "C0000001", Share: "1050.2500", Cost: domain.NewMoneyFromCents(100000), CurrentValue: domain.NewMoneyFromCents(100000),
	}}
	h := &Handlers{Svc: service.NewWealthService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/wealth/holdings?cust_id=C0000001&offset=5&limit=10")
	if code != 200 || !strings.Contains(body, `"cost":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
	if fake.gotCustID != "C0000001" || fake.gotOffset != 5 || fake.gotLimit != 10 {
		t.Errorf("参数透传错: %+v", fake)
	}
}

func TestGetHoldingProfile_OK(t *testing.T) {
	fake := &fakeWealthRepo{profile: &domain.WealthProfile{
		HoldingID: "WP-HD-0000001", CustID: "C0000001", ProductCode: "WP-FIX1", Share: "1050.2500",
		CurrentValue: domain.NewMoneyFromCents(100000), CustName: "张伟", CustType: "个人",
	}}
	h := &Handlers{Svc: service.NewWealthService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/wealth/holdings/WP-HD-0000001/profile")
	if code != 200 || !strings.Contains(body, "张伟") || !strings.Contains(body, `"current_value":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetHoldingProfile_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewWealthService(&fakeWealthRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/wealth/holdings/WP-HD-9999999/profile")
	if code != 404 {
		t.Errorf("code=%d want 404", code)
	}
}
```

- [ ] **Step 5: 写 cmd/wealth/main.go**

`templates/bank/cmd/wealth/main.go`:
```go
// Package main 是 wealth 只读 API 服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank/internal/platform/pg"
	"bank/internal/wealth/api"
	"bank/internal/wealth/repo"
	"bank/internal/wealth/service"
)

func main() {
	dbName := getenv("DB_NAME", "wealth_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Svc: service.NewWealthService(repo.NewWealthRepo(db)),
	}
	port := getenv("API_PORT", "8086")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("wealth 监听 :%s (db=%s)", port, dbName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

type pinger interface{ Ping() error }

func waitForDB(p pinger, retries int, wait time.Duration) error {
	var err error
	for i := 0; i < retries; i++ {
		if err = p.Ping(); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: 跑测试 + build**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/wealth/... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
```
Expected: PASS + build 成功（含 `cmd/wealth`）。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/wealth/service/ templates/bank/internal/wealth/api/ templates/bank/cmd/wealth/
git commit -m "feat(bank): wealth service+api+cmd 四层纵切 (B-4b)"
```

---

### Task 11: seed 编排接入 loan + wealth（8 步 → 10 步）

**Files:**
- Modify: `templates/bank/cmd/seed/main.go`
- Test: `templates/bank/cmd/seed/seed_test.go`（integration；扩断言）

**Interfaces:**
- Consumes: Task 4 的 `GenLoanStatic/WriteLoanStatic/RunLoan` + `LoanStatic`；Task 8 的 `GenWealthStatic/WriteWealthStatic/RunWealth` + `WealthStatic`；既有 `custIDs`（customer 步产出）、`demandNos`（core 步产出）。
- Produces: `runSeed` 10 步编排；`allDBs` 7 库。seed 完成后 loan_db/wealth_db 有全量数据且 FDW 外部表可 JOIN。

- [ ] **Step 1: main.go 加库与两步编排**

`templates/bank/cmd/seed/main.go` 共 5 处修改：

(a) `allDBs` 末尾追加两行：
```go
	{"loan_db", "db/migrations/loan_db.sql"},
	{"wealth_db", "db/migrations/wealth_db.sql"},
```

(b) `main()` 的完成日志改为：
```go
	log.Println("[seed] 完成 ✅（7 库 + core + customer + payment + reward + risk + loan + wealth + FDW）")
```

(c) `runSeed` 开头两步日志改为（计数 8→10）：
```go
	log.Println("[seed] 1/10 建 7 库")
	...
	log.Println("[seed] 2/10 建 7 库表")
```
后续 `3/8 core`…`7/8 risk` 五处日志相应改为 `3/10`…`7/10`（文字其余不变）。

(d) 在 risk 步（`riskDB.Close()` 之后）与 `setup_fdw` 之间插入：
```go
	log.Println("[seed] 8/10 loan")
	loanStatic := domains.GenLoanStatic(cfg, custIDs)
	loanDB, err := pg.Open("loan_db")
	if err != nil {
		return err
	}
	if err := domains.WriteLoanStatic(ctx, loanDB, loanStatic); err != nil {
		loanDB.Close()
		return err
	}
	if err := domains.RunLoan(ctx, loanDB, cfg, loanStatic.Accounts); err != nil {
		loanDB.Close()
		return err
	}
	loanDB.Close()

	log.Println("[seed] 9/10 wealth")
	wealthStatic := domains.GenWealthStatic(cfg, custIDs, demandNos)
	wealthDB, err := pg.Open("wealth_db")
	if err != nil {
		return err
	}
	if err := domains.WriteWealthStatic(ctx, wealthDB, wealthStatic); err != nil {
		wealthDB.Close()
		return err
	}
	if err := domains.RunWealth(ctx, wealthDB, cfg, wealthStatic.Products, wealthStatic.Holdings, custIDs, demandNos); err != nil {
		wealthDB.Close()
		return err
	}
	wealthDB.Close()
```

(e) setup_fdw 日志改为：
```go
	log.Println("[seed] 10/10 setup_fdw")
```

- [ ] **Step 2: seed_test.go 扩断言**

`templates/bank/cmd/seed/seed_test.go` 共 4 处修改：

(a) import 加 `"database/sql"`。

(b) `TestEnsureDBs_CreatesAllThree` 里两处 5 库名单改为 7 库（函数名保持不动）：
```go
[]string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db", "loan_db", "wealth_db"}
```

(c) `TestSeedRun_PopulatesAllDBs` 的非空表清单追加 10 行：
```go
		{"loan_db", "loan_product"}, {"loan_db", "loan_account"}, {"loan_db", "loan_repay"},
		{"loan_db", "loan_balance"}, {"loan_db", "loan_overdue"},
		{"wealth_db", "wealth_product"}, {"wealth_db", "wealth_nav"}, {"wealth_db", "wealth_holding"},
		{"wealth_db", "wealth_order"}, {"wealth_db", "wealth_income"},
```

(d) 函数末尾（`riskDB.Close()` 之后）追加 B-4b 断言段：
```go
	// B-4b: loan 逐日滚存——loan_balance 末日有快照；逾期五级分类可滑且档位合法；联邦可 JOIN
	loanDB, err := pg.Open("loan_db")
	if err != nil {
		t.Fatal(err)
	}
	var eodBal int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_balance WHERE biz_date='2026-07-13'").Scan(&eodBal); err != nil {
		t.Fatalf("查 loan_balance 末日: %v", err)
	}
	if eodBal == 0 {
		t.Error("loan_balance 末日(2026-07-13)无快照行")
	}
	var classN, badClass int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT overdue_class) FROM loan_overdue").Scan(&classN); err != nil {
		t.Fatalf("查 overdue_class: %v", err)
	}
	if classN < 2 {
		t.Errorf("逾期五级分类应随天数滑落至少 2 档, got %d", classN)
	}
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_overdue WHERE overdue_class NOT IN ('正常','关注','次级','可疑','损失')").Scan(&badClass); err != nil {
		t.Fatalf("查非法档位: %v", err)
	}
	if badClass > 0 {
		t.Errorf("loan_overdue 含非法五级分类 %d 行", badClass)
	}
	var loanCustName sql.NullString
	if err := loanDB.QueryRowContext(ctx, `SELECT ci.name FROM loan_account la
		JOIN ext_cust_db_cust_info ci ON la.cust_id=ci.cust_id LIMIT 1`).Scan(&loanCustName); err != nil {
		t.Errorf("loan_db 联邦 JOIN 不可查: %v", err)
	}
	loanDB.Close()
	// B-4b: wealth——nav 每产品每日有行；订单周末<工作日；income 覆盖全部持仓；联邦可 JOIN
	wealthDB, err := pg.Open("wealth_db")
	if err != nil {
		t.Fatal(err)
	}
	var navProds, navDays int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT product_code), COUNT(DISTINCT biz_date) FROM wealth_nav").Scan(&navProds, &navDays); err != nil {
		t.Fatalf("查 wealth_nav: %v", err)
	}
	if navProds != 6 || navDays < 400 {
		t.Errorf("wealth_nav 产品数=%d(应6) 天数=%d(应≥400)", navProds, navDays)
	}
	var wpWk, wpWd float64
	if err := wealthDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM wealth_order GROUP BY biz_date) q`).Scan(&wpWk, &wpWd); err != nil {
		t.Fatalf("查 wealth 周末/工作日均值: %v", err)
	}
	if wpWk == 0 || wpWk >= wpWd {
		t.Errorf("wealth 周末日均订单(%.0f) 应 < 工作日(%.0f)", wpWk, wpWd)
	}
	var incomeHoldings, incomeDays, holdingN int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT holding_id), COUNT(DISTINCT biz_date) FROM wealth_income").Scan(&incomeHoldings, &incomeDays); err != nil {
		t.Fatalf("查 wealth_income: %v", err)
	}
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM wealth_holding").Scan(&holdingN); err != nil {
		t.Fatalf("查 wealth_holding: %v", err)
	}
	if incomeHoldings != holdingN || incomeDays < 400 {
		t.Errorf("wealth_income 覆盖持仓 %d/%d 天数 %d(应≥400)", incomeHoldings, holdingN, incomeDays)
	}
	var wealthCustName sql.NullString
	if err := wealthDB.QueryRowContext(ctx, `SELECT ci.name FROM wealth_holding h
		JOIN ext_cust_db_cust_info ci ON h.cust_id=ci.cust_id LIMIT 1`).Scan(&wealthCustName); err != nil {
		t.Errorf("wealth_db 联邦 JOIN 不可查: %v", err)
	}
	wealthDB.Close()
```

- [ ] **Step 3: build + 真 pg 集成验证（临时 pg 5433）**

```bash
docker run -d --name bank-b4b-pg -e POSTGRES_USER=bank -e POSTGRES_PASSWORD=bank -e POSTGRES_DB=postgres -p 5433:5432 postgres:16
for i in $(seq 1 20); do docker exec bank-b4b-pg pg_isready -U bank >/dev/null 2>&1 && break; sleep 1; done
cd templates/bank && DB_PORT=5433 CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/...
```
Expected: PASS（runSeed 全量跑通：7 库建表 + 8 域灌数 + FDW；约 1-3 分钟）。
清理：`docker rm -f bank-b4b-pg`（这是本任务自建的临时容器，可删）。

若 `docker` 不可用或端口冲突：记录现象并在报告注明「集成验证未本地跑」，不得杀任何已有容器/进程。

- [ ] **Step 4: Commit**
```bash
git add templates/bank/cmd/seed/main.go templates/bank/cmd/seed/seed_test.go
git commit -m "feat(bank): seed 编排接入 loan/wealth（8→10 步，7 库） (B-4b)"
```

---

### Task 12: template.yaml + docker-compose + manifest_test + 重打包 + 全量验证

**Files:**
- Modify: `templates/bank/template.yaml`
- Modify: `templates/bank/docker-compose.yaml`
- Modify: `internal/template/manifest_test.go`（jiade 仓根）
- Repack: `internal/template/templates.tar`（`go generate` 产出）

**Interfaces:**
- Consumes: 全部前序任务（cmd/loan、cmd/wealth 可构建；db/migrations/{loan_db,wealth_db}.sql 存在）。
- Produces: `jiade init --template bank` 产出 7 服务 7 库工程（version 0.4.0）；spec §14 验收全绿。

- [ ] **Step 1: template.yaml 全文替换为**

`templates/bank/template.yaml`:
```yaml
name: bank
description: 简化版银行核心系统（core/customer/payment/reward/risk/loan/wealth 服务，Spec B-4b）
version: 0.4.0
databases:
  - name: core_db
    migrate: db/migrations/core_db.sql
  - name: cust_db
    migrate: db/migrations/cust_db.sql
  - name: pay_db
    migrate: db/migrations/pay_db.sql
  - name: reward_db
    migrate: db/migrations/reward_db.sql
  - name: risk_db
    migrate: db/migrations/risk_db.sql
  - name: loan_db
    migrate: db/migrations/loan_db.sql
  - name: wealth_db
    migrate: db/migrations/wealth_db.sql
services:
  - {name: core-banking, port: 8080, db: core_db}
  - {name: customer, port: 8081, db: cust_db}
  - {name: payment, port: 8082, db: pay_db}
  - {name: reward, port: 8083, db: reward_db}
  - {name: risk, port: 8084, db: risk_db}
  - {name: loan, port: 8085, db: loan_db}
  - {name: wealth, port: 8086, db: wealth_db}
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

- [ ] **Step 2: docker-compose.yaml 追加两服务**

`templates/bank/docker-compose.yaml` 在 `risk:` 服务块之后追加（逐字镜像 risk 定义）：
```yaml
  loan:
    build:
      context: .
      args:
        CMD: loan
    container_name: bank-loan
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: loan_db
      API_PORT: "8085"
    ports: ["8085:8085"]
    depends_on:
      postgres: {condition: service_healthy}
  wealth:
    build:
      context: .
      args:
        CMD: wealth
    container_name: bank-wealth
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: wealth_db
      API_PORT: "8086"
    ports: ["8086:8086"]
    depends_on:
      postgres: {condition: service_healthy}
```

- [ ] **Step 3: manifest_test.go 断言 5→7（jiade 仓根）**

`internal/template/manifest_test.go` 的 `TestManifest_Bank` 内 4 处替换：

(a) 注释 `// Spec B-4a：5 服务（core-banking:8080 / customer:8081 / payment:8082 / reward:8083 / risk:8084）。` → `// Spec B-4b：7 服务（+loan:8085 / wealth:8086）。`
(b) `if len(m.Services) != 5 {` → `if len(m.Services) != 7 {`，`t.Fatalf("services=%+v want 5", m.Services)` → `t.Fatalf("services=%+v want 7", m.Services)`
(c) `wantSvc := map[string]int{"core-banking": 8080, "customer": 8081, "payment": 8082, "reward": 8083, "risk": 8084}` → `wantSvc := map[string]int{"core-banking": 8080, "customer": 8081, "payment": 8082, "reward": 8083, "risk": 8084, "loan": 8085, "wealth": 8086}`
(d) 注释 `// Spec B-4a：5 库（core_db / cust_db / pay_db / reward_db / risk_db）。` → `// Spec B-4b：7 库（+loan_db / wealth_db）。`；`if len(m.Databases) != 5 {` → `!= 7 {`，`want 5` → `want 7`；`wantDB := map[string]bool{"core_db": true, "cust_db": true, "pay_db": true, "reward_db": true, "risk_db": true}` → 追加 `"loan_db": true, "wealth_db": true`。

- [ ] **Step 4: 重打包 templates.tar + jiade 根验证**

```bash
go generate ./internal/template
go build ./... && go test ./...
```
（jiade 仓根）Expected: build 成功、`internal/template` 的 manifest 测试 7/7 通过、其余测试全绿。

> 重打包前检查：`ls templates/bank/` 不应有任何 go build 残留二进制（如 `bank`、`seed`、`core-banking` 等无扩展名可执行文件）。若有，**不要删**——停下来在报告里列出，等用户确认。

- [ ] **Step 5: bank 模板全量单测**

```bash
cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./...
```
Expected: 全绿（不含 integration tag）。

- [ ] **Step 6: 全量集成测试（临时 pg 5433）**

```bash
docker run -d --name bank-b4b-pg -e POSTGRES_USER=bank -e POSTGRES_PASSWORD=bank -e POSTGRES_DB=postgres -p 5433:5432 postgres:16
for i in $(seq 1 20); do docker exec bank-b4b-pg pg_isready -U bank >/dev/null 2>&1 && break; sleep 1; done
cd templates/bank && DB_PORT=5433 CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./...
docker rm -f bank-b4b-pg
```
Expected: 全绿（seed_test 的 B-4b 断言段 + loan/wealth repo 集成测试 + fdw_test）。

- [ ] **Step 7: e2e 冒烟（生成物自包含验收，spec §14.3-14.7）**

前置：停掉占用 5432 的本地 postgres 容器（`docker stop <容器名>`；只停容器不动数据，事后 `docker start <容器名>` 恢复）。确认 8080-8086 未被占用：`lsof -nP -iTCP:8080-8086 -sTCP:LISTEN`；若被占（如 corpit），**不要杀进程**——记录并在报告注明，跳过 e2e。

```bash
# 在 jiade 仓根
go run ./cmd/jiade init --template bank --dir /tmp/mybank-b4b
# （若 /tmp/mybank-b4b 已存在，改用 /tmp/mybank-b4b-2，不要 rm 旧目录）
cd /tmp/mybank-b4b
docker compose up -d --build          # 首次构建 7 个 Go 服务，约 3-8 分钟
go run ./cmd/seed --scale=dev --reset
# 验收 #4：7 服务 healthz
for p in 8080 8081 8082 8083 8084 8085 8086; do echo -n "$p: "; curl -sf localhost:$p/healthz || echo FAIL; echo; done
# 验收 #5：两个联邦端点返回跨库数据
LOAN_NO=$(curl -s 'localhost:8085/api/v1/loan/accounts?limit=1' | grep -o '"loan_no":"[^"]*"' | head -1 | cut -d'"' -f4)
curl -s "localhost:8085/api/v1/loan/accounts/$LOAN_NO/profile"     # 应含 "cust_name"
HID=$(curl -s 'localhost:8086/api/v1/wealth/holdings?limit=1' | grep -o '"holding_id":"[^"]*"' | head -1 | cut -d'"' -f4)
curl -s "localhost:8086/api/v1/wealth/holdings/$HID/profile"       # 应含 "cust_name" + "cust_type"
# 抽查只读端点
curl -s 'localhost:8085/api/v1/loan/overdue?limit=3'
curl -s 'localhost:8085/api/v1/loan/balances?from=2026-07-10&to=2026-07-13&limit=3'
curl -s 'localhost:8086/api/v1/wealth/nav?product_code=WP-FIX1&from=2026-07-10&to=2026-07-13'
curl -s 'localhost:8086/api/v1/wealth/orders?limit=3'
curl -s 'localhost:8086/api/v1/wealth/incomes?limit=3'
# 收尾
docker compose down
docker start <容器名>
```
Expected: 7 个 healthz 全 `{"status":"ok"}`；两 profile 含客户姓名字段；各列表端点返回非空数据。`/tmp/mybank-b4b` 保留不删（供用户复查；删除需用户确认）。

- [ ] **Step 8: Commit**
```bash
git add templates/bank/template.yaml templates/bank/docker-compose.yaml internal/template/manifest_test.go internal/template/templates.tar
git commit -m "feat(bank): B-4b template.yaml + compose 加 loan/wealth（:8085/:8086），重打包"
```

---

## Self-Review（写后自查，已执行）

**1. Spec 覆盖**：
- §2.1-1 loan 服务 → Task 1/3/4/5/6 ✓；§2.1-2 wealth → Task 1/7/8/9/10 ✓
- §2.1-3 多库 7 库 + compose/template 2 服务 2 端口 → Task 11(allDBs)/12 ✓
- §2.1-4 FDW Mappings +2 + 各 1 联邦端点 → Task 2/5/9 ✓
- §2.1-5 滚存形态（loan_balance/wealth_nav 逐日全量快照）→ Task 4/8 ✓
- §2.1-6 测试与验收 → 各任务单测 + Task 11 seed_test + Task 12 全量 ✓
- §4 拓扑（7 进程 7 库、端口 8085/8086）→ Task 6/10/12 ✓
- §5 schema（DDL 零差异 + 补索引）→ Task 1 ✓
- §6 fixture（复用内核、rng 偏移 +40/+41/+50/+51、既定公式、Q1-B 每日利息、§6.5 有意偏离表）→ Task 2/4/8 ✓
- §7 FDW → Task 2（Mappings）+ Task 5/9（JOIN 端点）✓
- §8 分层 + 自带 Money → Task 3/5/6/7/9/10 ✓
- §9 端点清单（含 `NULLIF($1,'')::date`、limit<=0→50、写操作不做）→ Task 5/6/9/10 ✓
- §10 seed 编排 10 步 → Task 11 ✓
- §11 模板契约（template.yaml/compose/manifest_test/重打包）→ Task 12 ✓
- §12 测试策略（domain 副本 money_test、handlers_test 404/分页/DTO、生成器确定性/档位/周末<工作日、seed_test 6 类断言）→ Task 3/6/4/8/7/10/11 ✓
- §13 错误处理（FDW 透出、建库重试、partial 失败即整体失败、--reset、删除需确认）→ 沿用既有实现 + Global Constraints ✓
- §14 验收 8 条 → Task 12 Step 4-7 ✓（#6 确定性由 Task 4/8 单测 DeepEqual + Task 11 周末/滑落断言承担）
- §15 约束（自闭/自包含/int64 分/四层/module 边界/go1.22/5432-5433）→ Global Constraints ✓

**2. 占位符扫描**：无 TBD/TODO；每个代码步骤含完整代码；无「同 Task N」引用（每任务代码自足）。

**3. 类型一致性**：
- `LoanStatic{Products, Accounts, Disbursements}` / `GenLoanStatic(cfg, custIDs)` / `WriteLoanStatic(ctx, db, s)` / `RunLoan(ctx, db, cfg, accounts []domain.LoanAccount)` — Task 4 定义 = Task 11 消费 ✓
- `WealthStatic{Products, Holdings}` / `GenWealthStatic(cfg, custIDs, demandAccounts)` / `WriteWealthStatic(ctx, db, s)` / `RunWealth(ctx, db, cfg, products, holdings, custIDs, demandAccounts)` — Task 8 定义 = Task 11 消费 ✓
- repo 方法集（Task 5/9）= service 接口（Task 6/10）= handlers 调用 ✓
- domain 字段名（Task 3/7）= repo scan（Task 5/9）= fixtures 构造（Task 4/8）= DTO（Task 6/10）✓
- 词库名（Task 2）= fixtures 引用（Task 4/8）✓
- ID 前缀：`LN%07d`/`LN-DB-%07d`/`LN-RP-{compact}-%05d`/`LN-OD-{compact}-{loanNo}`/`WP-HD-%07d`/`WP-OD-{compact}-%05d`/`WP-IC-{compact}-%05d` 全计划一致 ✓

**已知有意取舍**（非遗漏）：
- `templates/bank/ARCHITECTURE.md` 的 FDW 章节不更新（B-4a 同样未更新，spec §11 未要求；留作后续 doc 债）。
- `templates/bank/Makefile` 的 `up` 目标仍只起 3 个老服务（B-4a 既有疏漏；验收走 `jiade up` = `docker compose up -d` 全量，不受影响）。不在本 spec 范围，不顺手改。
- wealth 订单 `share` 独立随机（有意保留的怪癖），不由 amount/nav 推导——忠实还原。
