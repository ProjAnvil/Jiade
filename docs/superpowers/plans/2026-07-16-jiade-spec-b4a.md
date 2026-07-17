# Jiade Spec B-4a Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 bank 模板里纵切 reward + risk 两个只读服务（reward_db 6 表 / risk_db 3 表，schema 忠实还原），用 B-2 的 distribution 三因子做 points_txn / risk_event 的逐日生成，每服务 1 个跨库 FDW JOIN 端点，使 `jiade init → up → seed` 后可 curl 五服务 healthz + 两个新联邦端点。

**Architecture:** 单 postgres 实例 5 库（core/cust/pay/reward/risk），每域一个独立 Go 进程（`cmd/<域>` + `internal/<域>/{domain,repo,service,api}` 四层，镜像 customer/payment）。静态表一次性生成；事件表逐日循环（复用 `domains` 包内 `bizdate.go` 的 `trendFactor/seasonalFactor/cyclicalFactor/bizDateRange/placeholders`，每日独立 rng + `pg.RunInTx` 内 DELETE 当日 + 批量 INSERT）。FDW `Mappings` 加 reward_db/risk_db ← cust_db.cust_info。

**Tech Stack:** Go 1.22 · database/sql · math/rand/v2 PCG · net/http + chi v5 · postgres_fdw · postgres（集成测试 `//go:build integration`）。

## Global Constraints

> 每个任务的隐含前置条件。逐字来自 spec §16 + B-1/B-2 既有约束。

- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`）；改后在 **jiade 根** 跑 `go generate ./internal/template` 重打包 `templates.tar`（Task 12）。
- **go 1.22**：bank module `go.mod` pin 1.22；本地验证 macOS 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0` 前缀。CI（Linux）直接 `go test`。
- **bank 命令在 `templates/bank/` 下跑**。
- **金额 int64 分，禁 float**：reward coupon 金额走自带 `domain.Money`（复制 payment），repo 分↔NUMERIC。risk 无金额（`risk_score`/`threshold` 作 NUMERIC 文本直存；`Float64()` 仅在生成期算 risk_score 字符串，不进 domain/金额）。
- **依赖方向向内** `api → service → repo → domain`；各域 `domain` 互不依赖；`repo` 不 import `service`。`fixtures/domains` 可 import 各 `domain`。
- **确定性**：每日独立 rng（`seed+偏移+ordinal`）+ 确定 ID（`RW-/RS-<dateCompact>-<seq>`，**非** uuid4——原实现用 uuid4 非确定，这里改确定性 ID 是有意偏离，保 Spec A 确定性原则）。rng 偏移：reward `+30`(静态)/`+31`(逐日)，risk `+32`(静态)/`+33`(逐日)。
- **FDW server host 统一 `localhost`**（pg 连自己实例的其他库），port 5432，user/pass bank/bank。
- **Dockerfile 已参数化** `ARG CMD` → `go build ./cmd/${CMD}`：加 reward/risk 只需建 `cmd/reward`、`cmd/risk` 目录 + compose `args: CMD`，**不改 Dockerfile**。
- **本地 5432 可能被占**：集成/e2e 若冲突，用 `DB_PORT=5433` + 临时容器（CI 无此问题）。
- **删除需确认**：本计划无删除既有代码操作（仅新增 + 末尾追加）。

## File Structure

**Create:**
- `templates/bank/db/migrations/reward_db.sql` — 6 表（points_acct/points_txn/coupon/coupon_usage/campaign/member_level）
- `templates/bank/db/migrations/risk_db.sql` — 3 表（risk_rule/risk_event/blacklist）
- `templates/bank/internal/reward/domain/{money.go,reward.go}` + `money_test.go` + `reward_test.go`
- `templates/bank/internal/reward/repo/reward_repo.go` + `reward_repo_test.go`（integration）
- `templates/bank/internal/reward/service/reward_service.go`
- `templates/bank/internal/reward/api/{handlers.go,router.go}` + `handlers_test.go`
- `templates/bank/cmd/reward/main.go`
- `templates/bank/internal/risk/domain/risk.go` + `risk_test.go`
- `templates/bank/internal/risk/repo/risk_repo.go` + `risk_repo_test.go`（integration）
- `templates/bank/internal/risk/service/risk_service.go`
- `templates/bank/internal/risk/api/{handlers.go,router.go}` + `handlers_test.go`
- `templates/bank/cmd/risk/main.go`
- `templates/bank/internal/fixtures/domains/{reward.go,risk.go}` + `reward_test.go`(纯) + `risk_test.go`(纯)

**Modify:**
- `templates/bank/internal/fixtures/rng.go` — +`Float64()` + reward/risk 词库
- `templates/bank/internal/fixtures/config.go` — +`ScaleFactor(Scale) float64`
- `templates/bank/internal/platform/fdw/fdw.go` — `Mappings` +2 条
- `templates/bank/cmd/seed/main.go` — `allDBs` +2；新增 step6 reward / step7 risk
- `templates/bank/cmd/seed/seed_test.go` — 5 库 + reward/risk 灌数据 + 周末<工作日断言
- `templates/bank/template.yaml` — +2 db +2 svc，version 0.3.0
- `templates/bank/docker-compose.yaml` — +reward(:8083) +risk(:8084)

**Repack:** jiade 根 `internal/template/templates.tar`（Task 12）

## Execution Tracks（fan-out 用）

- **Shared（顺序，先做）**：Task 1（schema）→ Task 2（rng/config/fdw 基建）。
- **Track R（reward，可并行）**：Task 3 → 4 → 5 → 6。文件域：`reward/*`、`fixtures/domains/reward*`。
- **Track K（risk，可并行）**：Task 7 → 8 → 9 → 10。文件域：`risk/*`、`fixtures/domains/risk*`。
- **Integration（两 Track 汇合后）**：Task 11（seed 编排）→ Task 12（template/compose/重打包/全量验证）。

Track R 与 Track K **文件不相交**（仅共享 `domains` 包的不同文件 + Task 2 基建），Task 2 完成后可并行。Task 11 依赖两 Track 完成（seed 引用双方 domain）。

---

### Task 1: reward_db + risk_db schema

**Files:**
- Create: `templates/bank/db/migrations/reward_db.sql`
- Create: `templates/bank/db/migrations/risk_db.sql`
- Test: `templates/bank/internal/platform/migrate/migrate_test.go`（追加）

**Interfaces:**
- Produces: 两个 schema 文件，被 Task 2 建表、Task 5/9 的 FDW `IMPORT FOREIGN SCHEMA cust_info` 的对端（cust_db 已存在）。

- [ ] **Step 1: 创建 reward_db.sql**（6 表，纯 `CREATE TABLE IF NOT EXISTS`）

`templates/bank/db/migrations/reward_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS points_acct (
    cust_id         TEXT PRIMARY KEY,
    points_balance  INTEGER DEFAULT 0,
    frozen_points   INTEGER DEFAULT 0,
    member_level    TEXT,
    update_biz_date DATE
);

CREATE TABLE IF NOT EXISTS points_txn (
    txn_id      TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    biz_date    DATE NOT NULL,
    points      INTEGER NOT NULL,
    direction   TEXT NOT NULL,
    source_type TEXT,
    ref_txn_id  TEXT,
    summary     TEXT
);
CREATE INDEX IF NOT EXISTS idx_points_txn_bizdate ON points_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_points_txn_cust ON points_txn(cust_id, biz_date);

CREATE TABLE IF NOT EXISTS coupon (
    coupon_id       TEXT PRIMARY KEY,
    cust_id         TEXT NOT NULL,
    campaign_id     TEXT,
    face_value      NUMERIC(18,2) NOT NULL,
    min_spend       NUMERIC(18,2) DEFAULT 0,
    status          TEXT DEFAULT 'issued',
    issue_biz_date  DATE,
    expire_date     DATE
);
CREATE INDEX IF NOT EXISTS idx_coupon_cust ON coupon(cust_id);

CREATE TABLE IF NOT EXISTS coupon_usage (
    usage_id       TEXT PRIMARY KEY,
    coupon_id      TEXT NOT NULL,
    biz_date       DATE NOT NULL,
    txn_id         TEXT,
    deduct_amount  NUMERIC(18,2),
    merchant_id    TEXT
);

CREATE TABLE IF NOT EXISTS campaign (
    campaign_id     TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    type            TEXT,
    start_biz_date  DATE,
    end_biz_date    DATE,
    budget          NUMERIC(18,2),
    used_budget     NUMERIC(18,2) DEFAULT 0,
    status          TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS member_level (
    level_code       TEXT PRIMARY KEY,
    level_name       TEXT NOT NULL,
    points_threshold INTEGER NOT NULL,
    benefits_json    TEXT
);
```

- [ ] **Step 2: 创建 risk_db.sql**（3 表）

`templates/bank/db/migrations/risk_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS risk_rule (
    rule_id        TEXT PRIMARY KEY,
    rule_name      TEXT NOT NULL,
    rule_type      TEXT,
    condition_json TEXT,
    threshold      NUMERIC(18,2),
    action         TEXT,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS risk_event (
    event_id      TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    cust_id       TEXT,
    account_no    TEXT,
    rule_id       TEXT,
    risk_score    NUMERIC(6,2),
    action_taken  TEXT,
    txn_ref       TEXT,
    summary       TEXT
);
CREATE INDEX IF NOT EXISTS idx_risk_event_bizdate ON risk_event(biz_date);
CREATE INDEX IF NOT EXISTS idx_risk_event_rule ON risk_event(rule_id, biz_date);

CREATE TABLE IF NOT EXISTS blacklist (
    list_id            TEXT PRIMARY KEY,
    cust_id            TEXT,
    entity_type        TEXT,
    reason             TEXT,
    effective_biz_date DATE,
    expire_date        DATE,
    status             TEXT DEFAULT 'active'
);
```

- [ ] **Step 3: 扩展 migrate_test 断言新 schema 可切分**

在 `templates/bank/internal/platform/migrate/migrate_test.go` 末尾追加：
```go
func TestSplitStatements_RewardRiskSchemas(t *testing.T) {
	for _, name := range []string{"reward_db.sql", "risk_db.sql"} {
		sql, err := os.ReadFile("../../../../db/migrations/" + name)
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
（若 `migrate_test.go` 未 import `os`/`strings`，补充 import。）

- [ ] **Step 4: 跑测试**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/platform/migrate/...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/db/migrations/reward_db.sql templates/bank/db/migrations/risk_db.sql templates/bank/internal/platform/migrate/migrate_test.go
git commit -m "feat(bank): add reward_db + risk_db schemas (B-4a)"
```

---

### Task 2: 共享基建——rng Float64 + 词库 + ScaleFactor + FDW Mappings

**Files:**
- Modify: `templates/bank/internal/fixtures/rng.go`
- Modify: `templates/bank/internal/fixtures/config.go`
- Modify: `templates/bank/internal/platform/fdw/fdw.go`
- Test: `templates/bank/internal/fixtures/rng_test.go`（追加）

**Interfaces:**
- Produces: `fixtures.(*RNG).Float64() float64`、`fixtures.ScaleFactor(Scale) float64`、新词库（`MemberLevelCodes`/`CampaignTypes`/`PointSources`/`RiskActions`/`RiskReasons`/`EntityTypes`）、`fdw.Mappings` 含 reward/risk←cust_info。Track R/K 的 fixture 生成器复用。

- [ ] **Step 1: rng.go 加 Float64 + reward/risk 词库**

在 `rng.go` 的 `Choice` 方法后追加：
```go
// Float64 返回 [0.0,1.0) 的确定性随机浮点（仅用于非金额生成，如 risk_score / factor 缩放）。
func (g *RNG) Float64() float64 { return g.r.Float64() }
```

在 `rng.go` 的 `var(...)` 词库块末尾（`Devices` 之后、右括号之前）追加：
```go
	// B-4a 新增词库
	MemberLevelCodes = []string{"L1", "L2", "L3", "L4", "L5"}
	CampaignTypes    = []string{"满减", "返现", "积分翻倍", "新客"}
	PointSources     = []string{"消费", "活动", "签到", "赎回"}
	PointDirections  = []string{"earn", "earn", "earn", "redeem"} // 3/4 earn
	RiskActions      = []string{"拦截", "放行", "人工"}
	RiskReasons      = []string{"欺诈", "洗钱嫌疑", "投诉涉诉"}
	EntityTypes      = []string{"客户"}
```

- [ ] **Step 2: config.go 加 ScaleFactor**

在 `config.go` 末尾追加：
```go
// ScaleFactor 返回规模缩放（full=1.0, dev=0.25）。
// reward/risk/loan/wealth 的每日量 = base × ScaleFactor × factor。
func ScaleFactor(s Scale) float64 {
	if s == ScaleFull {
		return 1.0
	}
	return 0.25
}
```

- [ ] **Step 3: fdw.go Mappings 加 2 条**

把 `fdw.go` 的 `Mappings`（约 21-26 行）末尾的 `}` 前追加两行：
```go
	{Host: "reward_db", Remote: "cust_db", Tables: []string{"cust_info"}}, // 原型已有
	{Host: "risk_db", Remote: "cust_db", Tables: []string{"cust_info"}},   // B-4a 新增
```
（即 `Mappings` 变为 6 条：core←cust、cust←core、pay←core、pay←cust、reward←cust、risk←cust。）

- [ ] **Step 4: rng_test.go 加 Float64 确定性测试**

在 `templates/bank/internal/fixtures/rng_test.go` 末尾追加（若文件存在；否则新建 `package fixtures` 的测试文件）：
```go
func TestRNG_Float64Deterministic(t *testing.T) {
	a := NewRNG(99).Float64()
	b := NewRNG(99).Float64()
	if a != b {
		t.Errorf("Float64 不确定: %v != %v", a, b)
	}
	if a < 0 || a >= 1 {
		t.Errorf("Float64 越界: %v", a)
	}
}

func TestScaleFactor(t *testing.T) {
	if ScaleFactor(ScaleDev) != 0.25 {
		t.Error("dev 应 0.25")
	}
	if ScaleFactor(ScaleFull) != 1.0 {
		t.Error("full 应 1.0")
	}
}
```

- [ ] **Step 5: 跑测试**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...
```
Expected: PASS（含新 Float64/ScaleFactor 测试）。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/fixtures/rng.go templates/bank/internal/fixtures/config.go templates/bank/internal/fixtures/rng_test.go templates/bank/internal/platform/fdw/fdw.go
git commit -m "feat(bank): B-4a 共享基建——RNG Float64 + 词库 + ScaleFactor + FDW reward/risk 映射"
```

---

### Task 3: reward domain（Money int64 分 + 模型）

**Files:**
- Create: `templates/bank/internal/reward/domain/money.go`
- Create: `templates/bank/internal/reward/domain/money_test.go`
- Create: `templates/bank/internal/reward/domain/reward.go`
- Create: `templates/bank/internal/reward/domain/reward_test.go`

**Interfaces:**
- Produces: `domain.Money`（复制 payment，int64 分，禁 float）、`domain.PointsAcct`/`PointsTxn`/`Coupon`/`Campaign`/`MemberLevel`/`RewardProfile`/`CouponStatus` 常量。Task 4/5 复用。

- [ ] **Step 1: 写 Money 测试（先失败）**

`templates/bank/internal/reward/domain/money_test.go`:
```go
package domain

import (
	"os"
	"strings"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	m := NewMoneyFromCents(123456)
	if m.String() != "1234.56" {
		t.Errorf("String=%s want 1234.56", m.String())
	}
	p, err := ParseCents("1234.56")
	if err != nil || p != m {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}

// TestSourceHasNoFloat 守卫 money.go 源码不含 float（金融禁 float）。
func TestSourceHasNoFloat(t *testing.T) {
	b, err := os.ReadFile("money.go")
	if err != nil {
		t.Fatal("读不到 money.go:", err)
	}
	if strings.Contains(string(b), "float") {
		t.Fatal("money.go 含 float，违反金融不变量")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/reward/domain/...`
Expected: FAIL（类型未定义）。

- [ ] **Step 3: 实现 money.go**（从 `payment/domain/money.go` 原样复制，package 为 `domain`）

`templates/bank/internal/reward/domain/money.go`:
```go
package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Money 用 int64 分表示金额。金融禁 float。构造仅经 NewMoneyFromCents 或 ParseCents。
type Money int64

func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents 把 NUMERIC(18,2) 字符串解析为分。纯整数运算，杜绝 float。
func ParseCents(s string) (Money, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("money: 小数位超过 2: %q", s)
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	n, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: 解析 %q 失败: %w", s, err)
	}
	if neg {
		n = -n
	}
	return Money(n), nil
}

func (m Money) Add(o Money) Money { return m + o }
func (m Money) Sub(o Money) Money { return m - o }
func (m Money) Cents() int64      { return int64(m) }

// String 返回 NUMERIC(18,2) 风格字符串（写 DB 用）。
func (m Money) String() string {
	n := int64(m)
	neg := n < 0
	if neg {
		n = -n
	}
	yuan, cents := n/100, n%100
	if neg {
		return fmt.Sprintf("-%d.%02d", yuan, cents)
	}
	return fmt.Sprintf("%d.%02d", yuan, cents)
}
```

- [ ] **Step 4: 写 reward 模型测试（先失败）**

`templates/bank/internal/reward/domain/reward_test.go`:
```go
package domain

import "testing"

func TestPointsAcct(t *testing.T) {
	a := PointsAcct{CustID: "C0000001", PointsBalance: 500, MemberLevel: "L2"}
	if a.CustID != "C0000001" || a.PointsBalance != 500 {
		t.Errorf("got %+v", a)
	}
}

func TestCouponMoney(t *testing.T) {
	c := Coupon{CouponID: "CP1", FaceValue: NewMoneyFromCents(2000), MinSpend: NewMoneyFromCents(5000)}
	if c.FaceValue.String() != "20.00" || c.MinSpend.String() != "50.00" {
		t.Errorf("coupon 金额错: %s %s", c.FaceValue, c.MinSpend)
	}
}
```

- [ ] **Step 5: 实现 reward.go 模型**

`templates/bank/internal/reward/domain/reward.go`:
```go
// Package domain 是 reward 服务的纯领域模型，零 DB/框架依赖（最内层）。
package domain

// PointsAcct 对应 points_acct 表。
type PointsAcct struct {
	CustID         string
	PointsBalance  int
	FrozenPoints   int
	MemberLevel    string
	UpdateBizDate  string
}

// PointsTxn 对应 points_txn 表。
type PointsTxn struct {
	TxnID      string
	CustID     string
	BizDate    string
	Points     int
	Direction  string // earn/redeem
	SourceType string
	RefTxnID   string
	Summary    string
}

// Coupon 对应 coupon 表（face_value/min_spend 为金额 int64 分）。
type Coupon struct {
	CouponID     string
	CustID       string
	CampaignID   string
	FaceValue    Money
	MinSpend     Money
	Status       string
	IssueBizDate string
	ExpireDate   string
}

// Campaign 对应 campaign 表（budget/used_budget 为金额）。
type Campaign struct {
	CampaignID    string
	Name          string
	Type          string
	StartBizDate  string
	EndBizDate    string
	Budget        Money
	UsedBudget    Money
	Status        string
}

// MemberLevel 对应 member_level 表。
type MemberLevel struct {
	LevelCode       string
	LevelName       string
	PointsThreshold int
	BenefitsJSON    string
}

// RewardProfile 是联邦查询结果（points_acct JOIN ext_cust_db_cust_info）。
type RewardProfile struct {
	CustID        string
	PointsBalance int
	MemberLevel   string
	CustName      string
	CustType      string
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/reward/domain/...`
Expected: PASS（含 float 守卫）。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/reward/domain/
git commit -m "feat(bank): reward domain (int64 Money + points/coupon/campaign models)"
```

---

### Task 4: reward fixture 生成器（静态 + 逐日三因子）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/reward.go`
- Create: `templates/bank/internal/fixtures/domains/reward_test.go`（纯单测）

**Interfaces:**
- Consumes: `fixtures.Config`/`RNG`/`ScaleFactor`/词库、`reward/domain`、`bizdate.go` 的 `trendFactor/seasonalFactor/cyclicalFactor/bizDateRange/dayOrdinal/dateCompact/placeholders`、`core.go` 的 `nullable`、`pg.RunInTx`/`pg.DBTX`。
- Produces: `type RewardStatic struct`、`GenRewardStatic(cfg, custIDs) RewardStatic`、`WriteRewardStatic(ctx,db,RewardStatic) error`、`RunReward(ctx,db,cfg,accts,campaignIDs) error`。Task 11 的 seed 调用。

- [ ] **Step 1: 写确定性测试（先失败）**

`templates/bank/internal/fixtures/domains/reward_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenRewardStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003"}
	a := GenRewardStatic(cfg, custIDs)
	b := GenRewardStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenRewardStatic 不确定")
	}
	if len(a.PointsAccts) != 3 || a.PointsAccts[0].CustID != "C0000001" {
		t.Errorf("points_acct 错: %+v", a.PointsAccts)
	}
	if len(a.MemberLevels) != 5 {
		t.Errorf("member_level 应 5 档, got %d", len(a.MemberLevels))
	}
}

func TestGenRewardStatic_ScaleCampaigns(t *testing.T) {
	dev := GenRewardStatic(fixtures.DefaultConfig(fixtures.ScaleDev), []string{"C1"})
	full := GenRewardStatic(fixtures.DefaultConfig(fixtures.ScaleFull), []string{"C1"})
	if len(full.Campaigns) <= len(dev.Campaigns) {
		t.Errorf("full campaigns(%d) 应 > dev(%d)", len(full.Campaigns), len(dev.Campaigns))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/domains/ -run TestGenRewardStatic`
Expected: FAIL（`GenRewardStatic` undefined）。

- [ ] **Step 3: 实现 reward.go**

`templates/bank/internal/fixtures/domains/reward.go`:
```go
package domains

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/reward/domain"
)

// reward 会员等级。
var rewardLevels = []struct {
	Code, Name string
	Threshold  int
}{
	{"L1", "普通", 0}, {"L2", "银卡", 10000}, {"L3", "金卡", 50000},
	{"L4", "白金", 200000}, {"L5", "钻石", 1000000},
}

// coupon 面额/门槛候选（元 → 分）。
var (
	couponFaceCents = []int{1000, 2000, 5000, 10000}
	couponMinCents  = []int{0, 5000, 10000}
)

// RewardStatic 静态表行集合（一次性生成）。
type RewardStatic struct {
	MemberLevels []domain.MemberLevel
	Campaigns    []domain.Campaign
	PointsAccts  []domain.PointsAcct
}

// GenRewardStatic 生成 member_level/campaign/points_acct。rng 偏移 +30。
func GenRewardStatic(cfg fixtures.Config, custIDs []string) RewardStatic {
	rng := fixtures.NewRNG(cfg.Seed + 30)
	sf := fixtures.ScaleFactor(cfg.Scale)

	var levels []domain.MemberLevel
	for i, lv := range rewardLevels {
		ben, _ := json.Marshal(map[string]float64{"discount": 0.01 * float64(i)})
		levels = append(levels, domain.MemberLevel{
			LevelCode: lv.Code, LevelName: lv.Name,
			PointsThreshold: lv.Threshold, BenefitsJSON: string(ben),
		})
	}

	nCamp := maxInt(3, int(12*sf))
	campaigns := make([]domain.Campaign, 0, nCamp)
	for i := 0; i < nCamp; i++ {
		start := fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate)
		end := addDays(start, rng.IntRange(7, 60))
		campaigns = append(campaigns, domain.Campaign{
			CampaignID: fmt.Sprintf("CP%04d", i), Name: rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "活动",
			Type: rng.Choice(fixtures.CampaignTypes), StartBizDate: start, EndBizDate: end,
			Budget: domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 10000),
			Status: "active",
		})
	}

	accts := make([]domain.PointsAcct, len(custIDs))
	for i, cid := range custIDs {
		accts[i] = domain.PointsAcct{
			CustID: cid, PointsBalance: rng.IntRange(0, 5000), FrozenPoints: 0,
			MemberLevel: rng.Choice(fixtures.MemberLevelCodes), UpdateBizDate: cfg.StartBizDate,
		}
	}
	return RewardStatic{MemberLevels: levels, Campaigns: campaigns, PointsAccts: accts}
}

// WriteRewardStatic 幂等写 member_level/campaign/points_acct（先 DELETE 后 INSERT）。
func WriteRewardStatic(ctx context.Context, db *sql.DB, s RewardStatic) error {
	for _, t := range []string{"points_acct", "campaign", "member_level"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, lv := range s.MemberLevels {
		if _, err := db.ExecContext(ctx, `INSERT INTO member_level(level_code,level_name,points_threshold,benefits_json)
			VALUES($1,$2,$3,$4)`, lv.LevelCode, lv.LevelName, lv.PointsThreshold, lv.BenefitsJSON); err != nil {
			return err
		}
	}
	for _, c := range s.Campaigns {
		if _, err := db.ExecContext(ctx, `INSERT INTO campaign(campaign_id,name,type,start_biz_date,end_biz_date,budget,used_budget,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			c.CampaignID, c.Name, c.Type, c.StartBizDate, c.EndBizDate,
			c.Budget.String(), c.UsedBudget.String(), c.Status); err != nil {
			return err
		}
	}
	for _, a := range s.PointsAccts {
		if _, err := db.ExecContext(ctx, `INSERT INTO points_acct(cust_id,points_balance,frozen_points,member_level,update_biz_date)
			VALUES($1,$2,$3,$4,$5)`,
			a.CustID, a.PointsBalance, a.FrozenPoints, a.MemberLevel, a.UpdateBizDate); err != nil {
			return err
		}
	}
	return nil
}

// RunReward 按 bizDate 推进生成 points_txn + coupon（逐日三因子 + 每日独立 rng seed+31+ordinal）。
// balances 内存滚存自静态 points_acct 初始余额（redeem 不超余额）。逐日不回写 points_acct。
func RunReward(ctx context.Context, db *sql.DB, cfg fixtures.Config, accts []domain.PointsAcct, campaignIDs []string) error {
	if len(accts) == 0 {
		return fmt.Errorf("reward: 无积分账户")
	}
	if len(campaignIDs) == 0 {
		campaignIDs = []string{""}
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("reward: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	balances := make(map[string]int, len(accts))
	custIDs := make([]string, len(accts))
	for i, a := range accts {
		balances[a.CustID] = a.PointsBalance
		custIDs[i] = a.CustID
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		n := maxInt(1, int(50*sf*factor))
		rng := fixtures.NewRNG(cfg.Seed + 31 + dayOrdinal(d, base))
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		txns := make([]domain.PointsTxn, 0, n)
		var coupons []domain.Coupon
		for i := 0; i < n; i++ {
			cid := custIDs[rng.IntRange(0, len(custIDs)-1)]
			direction := rng.Choice(fixtures.PointDirections)
			pts := rng.IntRange(10, 500)
			if direction == "redeem" {
				pts = minInt(pts, balances[cid])
				balances[cid] = maxInt(0, balances[cid]-pts)
			} else {
				balances[cid] += pts
			}
			txns = append(txns, domain.PointsTxn{
				TxnID: fmt.Sprintf("RW-PT-%s-%05d", compact, i), CustID: cid, BizDate: dateStr,
				Points: pts, Direction: direction, SourceType: rng.Choice(fixtures.PointSources),
			})
			if rng.IntRange(1, 20) == 1 { // 5% 发券
				coupons = append(coupons, domain.Coupon{
					CouponID: fmt.Sprintf("RW-CP-%s-%05d", compact, i), CustID: cid,
					CampaignID: rng.Choice(campaignIDs),
					FaceValue:  domain.NewMoneyFromCents(int64(couponFaceCents[rng.IntRange(0, len(couponFaceCents)-1)])),
					MinSpend:   domain.NewMoneyFromCents(int64(couponMinCents[rng.IntRange(0, len(couponMinCents)-1)])),
					Status:     "issued", IssueBizDate: dateStr, ExpireDate: dateStr,
				})
			}
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM points_txn WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 points_txn %s: %w", dateStr, err)
			}
			if err := bulkInsertPointsTxns(ctx, q, txns); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM coupon WHERE issue_biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 coupon %s: %w", dateStr, err)
			}
			return bulkInsertCoupons(ctx, q, coupons)
		}); err != nil {
			return fmt.Errorf("reward: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertPointsTxns 批量插 points_txn（8 列；ref_txn_id/summary nullable）。
func bulkInsertPointsTxns(ctx context.Context, q pg.DBTX, rows []domain.PointsTxn) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 8
	const stmt = "INSERT INTO points_txn(txn_id,cust_id,biz_date,points,direction,source_type,ref_txn_id,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, t := range chunk {
			args = append(args, t.TxnID, t.CustID, t.BizDate, t.Points, t.Direction,
				nullable(t.SourceType), nullable(t.RefTxnID), nullable(t.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("reward: 批量插 points_txn: %w", err)
		}
	}
	return nil
}

// bulkInsertCoupons 批量插 coupon（8 列）。空切片跳过。
func bulkInsertCoupons(ctx context.Context, q pg.DBTX, rows []domain.Coupon) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 8
	const stmt = "INSERT INTO coupon(coupon_id,cust_id,campaign_id,face_value,min_spend,status,issue_biz_date,expire_date) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, c := range chunk {
			args = append(args, c.CouponID, c.CustID, nullable(c.CampaignID), c.FaceValue.String(),
				c.MinSpend.String(), c.Status, c.IssueBizDate, c.ExpireDate)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("reward: 批量插 coupon: %w", err)
		}
	}
	return nil
}

// addDays 把 YYYY-MM-DD 加 n 天（n 可正可负）。
func addDays(dateStr string, n int) string {
	t, err := parseDate2(dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

> `parseDate`（返回 `time.Time`，零值表失败）已在 `bizdate.go` 定义；`addDays` 需要错误反馈，故另写 `parseDate2`（返回 error）。二者同包不冲突。`bizDateBatchSize`/`placeholders`/`trendFactor`/`seasonalFactor`/`cyclicalFactor`/`dayOrdinal`/`dateCompact`/`nullable` 均同包复用。

- [ ] **Step 4: 补 parseDate2 到 bizdate.go**

在 `bizdate.go` 的 `parseDate` 函数后追加：
```go
// parseDate2 解析 YYYY-MM-DD（返回 error，供 addDays 等需错误反馈处使用）。
func parseDate2(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}
```

- [ ] **Step 5: 跑纯单测**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/domains/ -run TestGenRewardStatic -v`
Expected: PASS。

- [ ] **Step 6: 全量纯单测（确认未破坏既有）**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: PASS。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/fixtures/domains/reward.go templates/bank/internal/fixtures/domains/reward_test.go templates/bank/internal/fixtures/domains/bizdate.go
git commit -m "feat(bank): reward fixture 生成器（静态 + 逐日三因子 points_txn/coupon）"
```

---

### Task 5: reward repo（本库查询 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/reward/repo/reward_repo.go`
- Create: `templates/bank/internal/reward/repo/reward_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `reward/domain`、`pg.Open("reward_db")`、Task 2 的 `ext_cust_db_cust_info` 外部表。
- Produces: `repo.RewardRepo`（`GetPointsAcct`/`ListPointsAccts`/`ListCoupons`/`GetProfile` FDW JOIN）。

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/internal/reward/repo/reward_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/reward/repo"
)

func setupRewardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("reward_db")
	if err != nil {
		t.Skipf("无 reward_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestRewardRepo_GetPointsAcct(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewRewardRepo(db)
	db.ExecContext(ctx, "DELETE FROM points_acct WHERE cust_id='IT-RC'")
	db.ExecContext(ctx, `INSERT INTO points_acct(cust_id,points_balance,frozen_points,member_level,update_biz_date)
		VALUES ('IT-RC',300,0,'L2','2026-01-01')`)
	got, err := r.GetPointsAcct(ctx, "IT-RC")
	if err != nil {
		t.Fatal(err)
	}
	if got.PointsBalance != 300 || got.MemberLevel != "L2" {
		t.Errorf("got %+v", got)
	}
}

func TestRewardRepo_ListPointsAccts(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	list, err := repo.NewRewardRepo(db).ListPointsAccts(context.Background(), "L2", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = list
}

func TestRewardRepo_ListCoupons(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	_, err := repo.NewRewardRepo(db).ListCoupons(context.Background(), "IT-RC", "", 0, 10)
	if err != nil {
		t.Fatalf("ListCoupons 失败: %v", err)
	}
}

func TestRewardRepo_GetProfile_FDWJoin(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err := repo.NewRewardRepo(db).GetProfile(context.Background(), "C0000001")
	if err != nil {
		t.Errorf("GetProfile FDW JOIN 失败（外部表未建？先 make seed）: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/reward/repo/...`
Expected: FAIL（`repo.NewRewardRepo` undefined）。

- [ ] **Step 3: 实现 repo**

`templates/bank/internal/reward/repo/reward_repo.go`:
```go
// Package repo 是 reward 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/reward/domain"
)

// RewardRepo reward 仓储。本库 points_acct/coupon 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type RewardRepo struct{ db *sql.DB }

// NewRewardRepo 构造 RewardRepo。
func NewRewardRepo(db *sql.DB) *RewardRepo { return &RewardRepo{db: db} }

// GetPointsAcct 查单个积分账户。不存在返回包装的 sql.ErrNoRows。
func (r *RewardRepo) GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error) {
	var a domain.PointsAcct
	var level sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT cust_id,points_balance,frozen_points,member_level,update_biz_date FROM points_acct WHERE cust_id=$1`,
		custID).Scan(&a.CustID, &a.PointsBalance, &a.FrozenPoints, &level, &a.UpdateBizDate)
	if err != nil {
		return domain.PointsAcct{}, fmt.Errorf("repo: 查积分账户 %s: %w", custID, err)
	}
	a.MemberLevel = level.String
	return a, nil
}

// ListPointsAccts 按 member_level 筛选（空则不限），分页。limit<=0 取 50。
func (r *RewardRepo) ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT cust_id,points_balance,frozen_points,member_level,update_biz_date FROM points_acct
		WHERE ($1='' OR member_level=$1) ORDER BY cust_id LIMIT $2 OFFSET $3`, memberLevel, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列积分账户: %w", err)
	}
	defer rows.Close()
	var out []domain.PointsAcct
	for rows.Next() {
		var a domain.PointsAcct
		var level sql.NullString
		if err := rows.Scan(&a.CustID, &a.PointsBalance, &a.FrozenPoints, &level, &a.UpdateBizDate); err != nil {
			return nil, fmt.Errorf("repo: 列积分账户 scan: %w", err)
		}
		a.MemberLevel = level.String
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListCoupons 查客户优惠券（status 空则不限），分页。
func (r *RewardRepo) ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT coupon_id,cust_id,campaign_id,face_value,min_spend,status,issue_biz_date,expire_date
		FROM coupon WHERE ($1='' OR cust_id=$1) AND ($2='' OR status=$2)
		ORDER BY issue_biz_date DESC, coupon_id LIMIT $3 OFFSET $4`, custID, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列优惠券: %w", err)
	}
	defer rows.Close()
	return scanCoupons(rows)
}

// GetProfile 跨库联邦：points_acct JOIN ext_cust_db_cust_info → 积分余额 + 会员等级 + 客户姓名/类型。
func (r *RewardRepo) GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	var p domain.RewardProfile
	var level, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT pa.cust_id, pa.points_balance, pa.member_level, ci.name, ci.cust_type
		FROM points_acct pa
		LEFT JOIN ext_cust_db_cust_info ci ON pa.cust_id=ci.cust_id
		WHERE pa.cust_id=$1`, custID).
		Scan(&p.CustID, &p.PointsBalance, &level, &name, &ctype)
	if err != nil {
		return domain.RewardProfile{}, fmt.Errorf("repo: 联邦查积分档案 %s: %w", custID, err)
	}
	p.MemberLevel, p.CustName, p.CustType = level.String, name.String, ctype.String
	return p, nil
}

func scanCoupons(rows *sql.Rows) ([]domain.Coupon, error) {
	var out []domain.Coupon
	for rows.Next() {
		var c domain.Coupon
		var camp, face, minS, issue, exp sql.NullString
		if err := rows.Scan(&c.CouponID, &c.CustID, &camp, &face, &minS, &c.Status, &issue, &exp); err != nil {
			return nil, fmt.Errorf("repo: scan 优惠券: %w", err)
		}
		c.CampaignID, c.IssueBizDate, c.ExpireDate = camp.String, issue.String, exp.String
		fv, err := domain.ParseCents(face.String)
		if err != nil {
			return nil, err
		}
		ms, err := domain.ParseCents(minS.String)
		if err != nil {
			return nil, err
		}
		c.FaceValue, c.MinSpend = fv, ms
		out = append(out, c)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试**

Run（需 postgres + `make seed` 含 setup_fdw）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/reward/repo/...
```
Expected: PASS（GetPointsAcct/List 通过；GetProfile/ListCoupons 不报错）。若 FDW JOIN 因 seed 未灌 reward 数据而无行仍 PASS（只断言不报错）。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/reward/repo/
git commit -m "feat(bank): reward repo with FDW cross-db join (reward profile)"
```

---

### Task 6: reward service + api + cmd

**Files:**
- Create: `templates/bank/internal/reward/service/reward_service.go`
- Create: `templates/bank/internal/reward/api/handlers.go`
- Create: `templates/bank/internal/reward/api/router.go`
- Create: `templates/bank/internal/reward/api/handlers_test.go`
- Create: `templates/bank/cmd/reward/main.go`

**Interfaces:**
- Consumes: Task 5 的 `repo.RewardRepo`。
- Produces: `reward` 服务进程（:8083）。

- [ ] **Step 1: 写 handler 单测（先失败）**

`templates/bank/internal/reward/api/handlers_test.go`:
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

	"bank/internal/reward/domain"
	"bank/internal/reward/service"
)

type fakeRewardRepo struct {
	acct    *domain.PointsAcct
	profile *domain.RewardProfile
	coupons []domain.Coupon
}

func (f fakeRewardRepo) GetPointsAcct(context.Context, string) (domain.PointsAcct, error) {
	if f.acct != nil {
		return *f.acct, nil
	}
	return domain.PointsAcct{}, sql.ErrNoRows
}
func (f fakeRewardRepo) ListPointsAccts(context.Context, string, int, int) ([]domain.PointsAcct, error) {
	return nil, nil
}
func (f fakeRewardRepo) ListCoupons(context.Context, string, string, int, int) ([]domain.Coupon, error) {
	return f.coupons, nil
}
func (f fakeRewardRepo) GetProfile(context.Context, string) (domain.RewardProfile, error) {
	if f.profile != nil {
		return *f.profile, nil
	}
	return domain.RewardProfile{}, sql.ErrNoRows
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
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetPointsAcct_OK(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{acct: &domain.PointsAcct{
		CustID: "C1", PointsBalance: 800, MemberLevel: "L3",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/reward/points-accounts/C1")
	if code != 200 || !strings.Contains(body, `"points_balance":800`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetPointsAcct_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/reward/points-accounts/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}

func TestGetProfile(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{profile: &domain.RewardProfile{
		CustID: "C1", PointsBalance: 800, MemberLevel: "L3", CustName: "张伟", CustType: "个人",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/reward/customers/C1/profile")
	if code != 200 || !strings.Contains(body, `"cust_name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/reward/api/...`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现 service**

`templates/bank/internal/reward/service/reward_service.go`:
```go
// Package service 是 reward 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/reward/domain"
)

// RewardStore reward 查询接口（repo 实现）。
type RewardStore interface {
	GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error)
	ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error)
	ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error)
	GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error)
}

// RewardService reward 只读服务，包装 RewardStore 做查询编排。
type RewardService struct{ store RewardStore }

// NewRewardService 构造 RewardService。
func NewRewardService(store RewardStore) *RewardService { return &RewardService{store: store} }

// GetPointsAcct 查单个积分账户。
func (s *RewardService) GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error) {
	return s.store.GetPointsAcct(ctx, custID)
}

// ListPointsAccts 按会员等级筛选并分页。
func (s *RewardService) ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error) {
	return s.store.ListPointsAccts(ctx, memberLevel, offset, limit)
}

// ListCoupons 查客户优惠券。
func (s *RewardService) ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error) {
	return s.store.ListCoupons(ctx, custID, status, offset, limit)
}

// Profile 查积分档案（跨库联邦）。
func (s *RewardService) Profile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	return s.store.GetProfile(ctx, custID)
}
```

- [ ] **Step 4: 实现 api（handlers + router）**

`templates/bank/internal/reward/api/handlers.go`:
```go
// Package api 是 reward 服务的传输层：http handlers + chi router。
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/reward/service"

	"github.com/go-chi/chi/v5"
)

// Handlers 持有 reward 只读服务。生产由 Svc 代理 repo；单测用
// service.NewRewardService(fakeRewardRepo) 注入。
type Handlers struct {
	Svc *service.RewardService
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GetPointsAcct 查单个积分账户。不存在返回 404。
func (h *Handlers) GetPointsAcct(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	a, err := h.Svc.GetPointsAcct(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("积分账户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, pointsAcctResp{
		CustID: a.CustID, PointsBalance: a.PointsBalance, FrozenPoints: a.FrozenPoints,
		MemberLevel: a.MemberLevel, UpdateBizDate: a.UpdateBizDate,
	})
}

// ListPointsAccts 按会员等级筛选并分页（query: member_level/offset/limit）。
func (h *Handlers) ListPointsAccts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListPointsAccts(r.Context(), q.Get("member_level"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]pointsAcctResp, 0, len(list))
	for _, a := range list {
		out = append(out, pointsAcctResp{
			CustID: a.CustID, PointsBalance: a.PointsBalance, MemberLevel: a.MemberLevel,
			UpdateBizDate: a.UpdateBizDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"points_accounts": out})
}

// ListCoupons 查客户优惠券（query: status/offset/limit）。
func (h *Handlers) ListCoupons(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	id := chi.URLParam(r, "cust_id")
	list, err := h.Svc.ListCoupons(r.Context(), id, q.Get("status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]couponResp, 0, len(list))
	for _, c := range list {
		out = append(out, couponResp{
			CouponID: c.CouponID, CustID: c.CustID, CampaignID: c.CampaignID,
			FaceValue: c.FaceValue.String(), MinSpend: c.MinSpend.String(),
			Status: c.Status, IssueBizDate: c.IssueBizDate, ExpireDate: c.ExpireDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"coupons": out})
}

// GetProfile 查积分档案（跨库联邦 JOIN）。
func (h *Handlers) GetProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	p, err := h.Svc.Profile(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("积分账户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, profileResp{
		CustID: p.CustID, PointsBalance: p.PointsBalance, MemberLevel: p.MemberLevel,
		CustName: p.CustName, CustType: p.CustType,
	})
}

// --- DTO ---

type pointsAcctResp struct {
	CustID        string `json:"cust_id"`
	PointsBalance int    `json:"points_balance"`
	FrozenPoints  int    `json:"frozen_points,omitempty"`
	MemberLevel   string `json:"member_level,omitempty"`
	UpdateBizDate string `json:"update_biz_date,omitempty"`
}

type couponResp struct {
	CouponID     string `json:"coupon_id"`
	CustID       string `json:"cust_id"`
	CampaignID   string `json:"campaign_id,omitempty"`
	FaceValue    string `json:"face_value"`
	MinSpend     string `json:"min_spend"`
	Status       string `json:"status,omitempty"`
	IssueBizDate string `json:"issue_biz_date,omitempty"`
	ExpireDate   string `json:"expire_date,omitempty"`
}

type profileResp struct {
	CustID        string `json:"cust_id"`
	PointsBalance int    `json:"points_balance"`
	MemberLevel   string `json:"member_level,omitempty"`
	CustName      string `json:"cust_name,omitempty"`
	CustType      string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
```

`templates/bank/internal/reward/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 reward 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/reward/points-accounts", h.ListPointsAccts)
		r.Get("/reward/points-accounts/{cust_id}", h.GetPointsAcct)
		r.Get("/reward/customers/{cust_id}/coupons", h.ListCoupons)
		r.Get("/reward/customers/{cust_id}/profile", h.GetProfile)
	})
	return r
}
```

- [ ] **Step 5: 实现 cmd/reward/main.go**（仿 customer/main.go，库名 reward_db、端口 8083）

`templates/bank/cmd/reward/main.go`:
```go
// Package main 是 reward 只读 API 服务入口。
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
	"bank/internal/reward/api"
	"bank/internal/reward/repo"
	"bank/internal/reward/service"
)

func main() {
	dbName := getenv("DB_NAME", "reward_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Svc: service.NewRewardService(repo.NewRewardRepo(db)),
	}
	port := getenv("API_PORT", "8083")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("reward 监听 :%s (db=%s)", port, dbName)
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

Run:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/reward/...
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./cmd/reward
```
Expected: 测试 PASS；build 成功。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/reward/service/ templates/bank/internal/reward/api/ templates/bank/cmd/reward/
git commit -m "feat(bank): reward service + api + cmd (read-only, :8083)"
```

---

### Task 7: risk domain（无 Money）

**Files:**
- Create: `templates/bank/internal/risk/domain/risk.go`
- Create: `templates/bank/internal/risk/domain/risk_test.go`

**Interfaces:**
- Produces: `domain.RiskRule`/`RiskEvent`/`Blacklist`/`RiskEventDetail`（无 Money；risk_score/threshold 为 string）。Task 8/9 复用。

- [ ] **Step 1: 写失败测试**

`templates/bank/internal/risk/domain/risk_test.go`:
```go
package domain

import "testing"

func TestRiskEvent(t *testing.T) {
	e := RiskEvent{EventID: "E1", CustID: "C1", RiskScore: "0.73", ActionTaken: "拦截"}
	if e.CustID != "C1" || e.RiskScore != "0.73" {
		t.Errorf("got %+v", e)
	}
}

func TestRiskRule(t *testing.T) {
	r := RiskRule{RuleID: "R001", Action: "拦截", Threshold: "100000.00"}
	if r.Action != "拦截" {
		t.Errorf("got %+v", r)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/risk/domain/...`
Expected: FAIL（类型未定义）。

- [ ] **Step 3: 实现 risk.go**

`templates/bank/internal/risk/domain/risk.go`:
```go
// Package domain 是 risk 服务的纯领域模型，零 DB/框架依赖（最内层）。
// risk 无金额字段：risk_score/threshold 作 NUMERIC 文本直存（不引入 Money）。
package domain

// RiskRule 对应 risk_rule 表。
type RiskRule struct {
	RuleID        string
	RuleName      string
	RuleType      string
	ConditionJSON string
	Threshold     string // NUMERIC(18,2) 文本，非金额（通用阈值）
	Action        string
	Status        string
}

// RiskEvent 对应 risk_event 表。
type RiskEvent struct {
	EventID     string
	BizDate     string
	CustID      string
	AccountNo   string
	RuleID      string
	RiskScore   string // NUMERIC(6,2) 文本（0.30~0.95），非金额
	ActionTaken string
	TxnRef      string
	Summary     string
}

// Blacklist 对应 blacklist 表。
type Blacklist struct {
	ListID           string
	CustID           string
	EntityType       string
	Reason           string
	EffectiveBizDate string
	ExpireDate       string
	Status           string
}

// RiskEventDetail 是联邦查询结果（risk_event JOIN ext_cust_db_cust_info）。
type RiskEventDetail struct {
	RiskEvent
	CustName string
	CustType string
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/risk/domain/...`
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/risk/domain/
git commit -m "feat(bank): risk domain model (no money; score/threshold as text)"
```

---

### Task 8: risk fixture 生成器（静态 + 逐日三因子）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/risk.go`
- Create: `templates/bank/internal/fixtures/domains/risk_test.go`（纯单测）

**Interfaces:**
- Consumes: `fixtures.Config`/`RNG`/`ScaleFactor`/`Float64`/词库、`risk/domain`、`bizdate.go` 的 `trendFactor/seasonalFactor/cyclicalFactor/bizDateRange/dayOrdinal/dateCompact/placeholders`、`core.go` 的 `nullable`、`reward.go` 的 `parseDate2`（如需）、`pg.RunInTx`/`pg.DBTX`。
- Produces: `type RiskStatic struct`、`GenRiskStatic(cfg, custIDs) RiskStatic`、`WriteRiskStatic(ctx,db,RiskStatic) error`、`RunRisk(ctx,db,cfg,custIDs,accountNos) error`。

- [ ] **Step 1: 写确定性测试（先失败）**

`templates/bank/internal/fixtures/domains/risk_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenRiskStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002"}
	a := GenRiskStatic(cfg, custIDs)
	b := GenRiskStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenRiskStatic 不确定")
	}
	if len(a.Rules) != 5 {
		t.Errorf("risk_rule 应 5 条, got %d", len(a.Rules))
	}
	if a.Rules[0].RuleID != "R001" {
		t.Errorf("首规则 id=%s", a.Rules[0].RuleID)
	}
}

func TestGenRiskStatic_ScaleBlacklist(t *testing.T) {
	dev := GenRiskStatic(fixtures.DefaultConfig(fixtures.ScaleDev), []string{"C1"})
	full := GenRiskStatic(fixtures.DefaultConfig(fixtures.ScaleFull), []string{"C1"})
	if len(full.Blacklists) <= len(dev.Blacklists) {
		t.Errorf("full blacklist(%d) 应 > dev(%d)", len(full.Blacklists), len(dev.Blacklists))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/domains/ -run TestGenRiskStatic`
Expected: FAIL（`GenRiskStatic` undefined）。

- [ ] **Step 3: 实现 risk.go**

`templates/bank/internal/fixtures/domains/risk.go`:
```go
package domains

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/risk/domain"
)

// risk 规则。field/op/threshold 编进 condition_json。
var riskRules = []struct {
	ID, Name, Field string
	Threshold       int
	Action          string
}{
	{"R001", "单笔大额转账", "amount", 100000, "拦截"},
	{"R002", "频繁交易", "count_5min", 10, "人工"},
	{"R003", "异地登录", "region_mismatch", 1, "放行"},
	{"R004", "非工作时间大额", "amount+hour", 50000, "人工"},
	{"R005", "黑名单命中", "blacklist", 1, "拦截"},
}

// RiskStatic 静态表行集合。
type RiskStatic struct {
	Rules      []domain.RiskRule
	Blacklists []domain.Blacklist
}

// GenRiskStatic 生成 risk_rule + blacklist。rng 偏移 +32。
func GenRiskStatic(cfg fixtures.Config, custIDs []string) RiskStatic {
	rng := fixtures.NewRNG(cfg.Seed + 32)
	sf := fixtures.ScaleFactor(cfg.Scale)

	rules := make([]domain.RiskRule, len(riskRules))
	for i, rr := range riskRules {
		cond, _ := json.Marshal(map[string]any{"field": rr.Field, "op": ">=", "threshold": rr.Threshold})
		rules[i] = domain.RiskRule{
			RuleID: rr.ID, RuleName: rr.Name, RuleType: "transaction",
			ConditionJSON: string(cond), Threshold: fmt.Sprintf("%d.00", rr.Threshold),
			Action: rr.Action, Status: "active",
		}
	}

	blCount := maxInt(2, int(20*sf))
	blacklists := make([]domain.Blacklist, blCount)
	for i := 0; i < blCount; i++ {
		blacklists[i] = domain.Blacklist{
			ListID: fmt.Sprintf("RS-BL-%04d", i), CustID: pickStr(rng, custIDs),
			EntityType: rng.Choice(fixtures.EntityTypes), Reason: rng.Choice(fixtures.RiskReasons),
			EffectiveBizDate: cfg.StartBizDate, ExpireDate: cfg.EndBizDate, Status: "active",
		}
	}
	return RiskStatic{Rules: rules, Blacklists: blacklists}
}

// WriteRiskStatic 幂等写 risk_rule + blacklist（先 DELETE 后 INSERT）。
func WriteRiskStatic(ctx context.Context, db *sql.DB, s RiskStatic) error {
	for _, t := range []string{"blacklist", "risk_rule"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, r := range s.Rules {
		if _, err := db.ExecContext(ctx, `INSERT INTO risk_rule(rule_id,rule_name,rule_type,condition_json,threshold,action,status)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			r.RuleID, r.RuleName, r.RuleType, r.ConditionJSON, nullable(r.Threshold), r.Action, r.Status); err != nil {
			return err
		}
	}
	for _, b := range s.Blacklists {
		if _, err := db.ExecContext(ctx, `INSERT INTO blacklist(list_id,cust_id,entity_type,reason,effective_biz_date,expire_date,status)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			b.ListID, nullable(b.CustID), b.EntityType, b.Reason, b.EffectiveBizDate, b.ExpireDate, b.Status); err != nil {
			return err
		}
	}
	return nil
}

// RunRisk 按 bizDate 推进生成 risk_event（逐日三因子 + 每日独立 rng seed+33+ordinal）。
func RunRisk(ctx context.Context, db *sql.DB, cfg fixtures.Config, custIDs, accountNos []string) error {
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("risk: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	ruleIDs := make([]string, len(riskRules))
	for i, r := range riskRules {
		ruleIDs[i] = r.ID
	}
	if len(custIDs) == 0 {
		custIDs = []string{""}
	}
	if len(accountNos) == 0 {
		accountNos = []string{""}
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		n := maxInt(0, int(5*sf*factor))
		rng := fixtures.NewRNG(cfg.Seed + 33 + dayOrdinal(d, base))
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		events := make([]domain.RiskEvent, 0, n)
		for i := 0; i < n; i++ {
			events = append(events, domain.RiskEvent{
				EventID: fmt.Sprintf("RS-EV-%s-%05d", compact, i), BizDate: dateStr,
				CustID: pickStr(rng, custIDs), AccountNo: pickStr(rng, accountNos),
				RuleID: pickStr(rng, ruleIDs),
				RiskScore: fmt.Sprintf("%.2f", 0.3+rng.Float64()*0.65),
				ActionTaken: rng.Choice(fixtures.RiskActions),
				TxnRef:  fmt.Sprintf("RS-TX-%s-%05d", compact, i),
				Summary: "触发规则 " + pickStr(rng, ruleIDs),
			})
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM risk_event WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 risk_event %s: %w", dateStr, err)
			}
			return bulkInsertRiskEvents(ctx, q, events)
		}); err != nil {
			return fmt.Errorf("risk: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertRiskEvents 批量插 risk_event（9 列；cust/account/rule/txn_ref/summary nullable）。
func bulkInsertRiskEvents(ctx context.Context, q pg.DBTX, rows []domain.RiskEvent) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 9
	const stmt = "INSERT INTO risk_event(event_id,biz_date,cust_id,account_no,rule_id,risk_score,action_taken,txn_ref,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, e := range chunk {
			args = append(args, e.EventID, e.BizDate, nullable(e.CustID), nullable(e.AccountNo),
				nullable(e.RuleID), nullable(e.RiskScore), nullable(e.ActionTaken),
				nullable(e.TxnRef), nullable(e.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("risk: 批量插 risk_event: %w", err)
		}
	}
	return nil
}

// pickStr 从 list 随机选一个（空 list 返回 ""）。
func pickStr(rng *fixtures.RNG, list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[rng.IntRange(0, len(list)-1)]
}
```

- [ ] **Step 4: 跑纯单测**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/domains/ -run TestGenRiskStatic -v`
Expected: PASS。

- [ ] **Step 5: 全量纯单测**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: PASS。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/fixtures/domains/risk.go templates/bank/internal/fixtures/domains/risk_test.go
git commit -m "feat(bank): risk fixture 生成器（静态 + 逐日三因子 risk_event）"
```

---

### Task 9: risk repo（本库查询 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/risk/repo/risk_repo.go`
- Create: `templates/bank/internal/risk/repo/risk_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `risk/domain`、`pg.Open("risk_db")`、Task 2 的 `ext_cust_db_cust_info`。
- Produces: `repo.RiskRepo`（`ListEvents`/`GetEvent` FDW JOIN/`ListRules`/`ListBlacklists`）。

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/internal/risk/repo/risk_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/risk/repo"
)

func setupRiskDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("risk_db")
	if err != nil {
		t.Skipf("无 risk_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestRiskRepo_ListRules(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	rules, err := repo.NewRiskRepo(db).ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Error("ListRules 应返回规则（seed 后有 5 条）")
	}
}

func TestRiskRepo_ListEventsAndBlacklists(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewRiskRepo(db)
	if _, err := r.ListEvents(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListEvents 失败: %v", err)
	}
	if _, err := r.ListBlacklists(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListBlacklists 失败: %v", err)
	}
}

func TestRiskRepo_GetEvent_FDWJoin(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err := repo.NewRiskRepo(db).GetEvent(context.Background(), "RS-EV-NOPE")
	if err == nil {
		t.Error("不存在的事件应返回错误")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/risk/repo/...`
Expected: FAIL（`repo.NewRiskRepo` undefined）。

- [ ] **Step 3: 实现 repo**

`templates/bank/internal/risk/repo/risk_repo.go`:
```go
// Package repo 是 risk 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/risk/domain"
)

// RiskRepo risk 仓储。本库 risk_event/risk_rule/blacklist 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type RiskRepo struct{ db *sql.DB }

// NewRiskRepo 构造 RiskRepo。
func NewRiskRepo(db *sql.DB) *RiskRepo { return &RiskRepo{db: db} }

// ListEvents 按日期/规则/action 筛选（空则不限），分页。
func (r *RiskRepo) ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT event_id,biz_date,cust_id,account_no,rule_id,risk_score,action_taken,txn_ref,summary
		FROM risk_event WHERE ($1='' OR biz_date>=$1) AND ($2='' OR biz_date<=$2)
		AND ($3='' OR rule_id=$3) AND ($4='' OR action_taken=$4)
		ORDER BY biz_date DESC, event_id LIMIT $5 OFFSET $6`, from, to, ruleID, action, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列风控事件: %w", err)
	}
	defer rows.Close()
	var out []domain.RiskEvent
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列风控事件 scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetEvent 跨库联邦：risk_event JOIN ext_cust_db_cust_info → 事件详情 + 客户姓名/类型。
func (r *RiskRepo) GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	q := `SELECT e.event_id,e.biz_date,e.cust_id,e.account_no,e.rule_id,e.risk_score,e.action_taken,e.txn_ref,e.summary,ci.name,ci.cust_type
		FROM risk_event e
		LEFT JOIN ext_cust_db_cust_info ci ON e.cust_id=ci.cust_id
		WHERE e.event_id=$1`
	var d domain.RiskEventDetail
	var cust, acct, rule, score, action, txnRef, summary, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx, q, eventID).Scan(
		&d.EventID, &d.BizDate, &cust, &acct, &rule, &score, &action, &txnRef, &summary, &name, &ctype)
	if err != nil {
		return domain.RiskEventDetail{}, fmt.Errorf("repo: 联邦查风控事件 %s: %w", eventID, err)
	}
	d.CustID, d.AccountNo, d.RuleID, d.RiskScore = cust.String, acct.String, rule.String, score.String
	d.ActionTaken, d.TxnRef, d.Summary, d.CustName, d.CustType = action.String, txnRef.String, summary.String, name.String, ctype.String
	return d, nil
}

// ListRules 列风控规则（静态）。
func (r *RiskRepo) ListRules(ctx context.Context) ([]domain.RiskRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rule_id,rule_name,rule_type,condition_json,threshold,action,status FROM risk_rule ORDER BY rule_id`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列风控规则: %w", err)
	}
	defer rows.Close()
	var out []domain.RiskRule
	for rows.Next() {
		var rule domain.RiskRule
		var cond, threshold sql.NullString
		if err := rows.Scan(&rule.RuleID, &rule.RuleName, &rule.RuleType, &cond, &threshold, &rule.Action, &rule.Status); err != nil {
			return nil, fmt.Errorf("repo: 列风控规则 scan: %w", err)
		}
		rule.ConditionJSON, rule.Threshold = cond.String, threshold.String
		out = append(out, rule)
	}
	return out, rows.Err()
}

// ListBlacklists 按客户筛选（空则不限），分页。
func (r *RiskRepo) ListBlacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT list_id,cust_id,entity_type,reason,effective_biz_date,expire_date,status
		FROM blacklist WHERE ($1='' OR cust_id=$1) ORDER BY list_id LIMIT $2 OFFSET $3`, custID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列黑名单: %w", err)
	}
	defer rows.Close()
	var out []domain.Blacklist
	for rows.Next() {
		var b domain.Blacklist
		var cust, reason sql.NullString
		if err := rows.Scan(&b.ListID, &cust, &b.EntityType, &reason, &b.EffectiveBizDate, &b.ExpireDate, &b.Status); err != nil {
			return nil, fmt.Errorf("repo: 列黑名单 scan: %w", err)
		}
		b.CustID, b.Reason = cust.String, reason.String
		out = append(out, b)
	}
	return out, rows.Err()
}

// scanEvent 扫描单行 risk_event（scan 函数由 QueryRow 或 Rows 注入）。
func scanEvent(scan func(dest ...any) error) (domain.RiskEvent, error) {
	var e domain.RiskEvent
	var cust, acct, rule, score, action, txnRef, summary sql.NullString
	if err := scan(&e.EventID, &e.BizDate, &cust, &acct, &rule, &score, &action, &txnRef, &summary); err != nil {
		return domain.RiskEvent{}, err
	}
	e.CustID, e.AccountNo, e.RuleID, e.RiskScore = cust.String, acct.String, rule.String, score.String
	e.ActionTaken, e.TxnRef, e.Summary = action.String, txnRef.String, summary.String
	return e, nil
}
```

- [ ] **Step 4: 跑测试**

Run（需 postgres + `make seed` 含 setup_fdw）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/risk/repo/...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/risk/repo/
git commit -m "feat(bank): risk repo with FDW cross-db join (event detail)"
```

---

### Task 10: risk service + api + cmd

**Files:**
- Create: `templates/bank/internal/risk/service/risk_service.go`
- Create: `templates/bank/internal/risk/api/handlers.go`
- Create: `templates/bank/internal/risk/api/router.go`
- Create: `templates/bank/internal/risk/api/handlers_test.go`
- Create: `templates/bank/cmd/risk/main.go`

**Interfaces:**
- Consumes: Task 9 的 `repo.RiskRepo`。
- Produces: `risk` 服务进程（:8084）。

- [ ] **Step 1: 写 handler 单测（先失败）**

`templates/bank/internal/risk/api/handlers_test.go`:
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

	"bank/internal/risk/domain"
	"bank/internal/risk/service"
)

type fakeRiskRepo struct {
	detail *domain.RiskEventDetail
	rules  []domain.RiskRule
}

func (f fakeRiskRepo) ListEvents(context.Context, string, string, string, string, int, int) ([]domain.RiskEvent, error) {
	return nil, nil
}
func (f fakeRiskRepo) GetEvent(context.Context, string) (domain.RiskEventDetail, error) {
	if f.detail != nil {
		return *f.detail, nil
	}
	return domain.RiskEventDetail{}, sql.ErrNoRows
}
func (f fakeRiskRepo) ListRules(context.Context) ([]domain.RiskRule, error) { return f.rules, nil }
func (f fakeRiskRepo) ListBlacklists(context.Context, string, int, int) ([]domain.Blacklist, error) {
	return nil, nil
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
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetEvent_OK(t *testing.T) {
	d := &domain.RiskEventDetail{}
	d.EventID = "E1"
	d.CustID = "C1"
	d.RiskScore = "0.73"
	d.ActionTaken = "拦截"
	d.CustName = "张伟"
	h := &Handlers{Svc: service.NewRiskService(fakeRiskRepo{detail: d})}
	code, body := get(t, NewRouter(h), "/api/v1/risk/events/E1")
	if code != 200 || !strings.Contains(body, `"cust_name":"张伟"`) || !strings.Contains(body, `"risk_score":"0.73"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetEvent_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewRiskService(fakeRiskRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/risk/events/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/risk/api/...`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现 service**

`templates/bank/internal/risk/service/risk_service.go`:
```go
// Package service 是 risk 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/risk/domain"
)

// RiskStore risk 查询接口（repo 实现）。
type RiskStore interface {
	ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error)
	GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error)
	ListRules(ctx context.Context) ([]domain.RiskRule, error)
	ListBlacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error)
}

// RiskService risk 只读服务，包装 RiskStore 做查询编排。
type RiskService struct{ store RiskStore }

// NewRiskService 构造 RiskService。
func NewRiskService(store RiskStore) *RiskService { return &RiskService{store: store} }

// ListEvents 按条件筛选风控事件并分页。
func (s *RiskService) ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error) {
	return s.store.ListEvents(ctx, from, to, ruleID, action, offset, limit)
}

// Event 查风控事件详情（跨库联邦）。
func (s *RiskService) Event(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	return s.store.GetEvent(ctx, eventID)
}

// Rules 列风控规则。
func (s *RiskService) Rules(ctx context.Context) ([]domain.RiskRule, error) { return s.store.ListRules(ctx) }

// Blacklists 按客户筛选黑名单并分页。
func (s *RiskService) Blacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error) {
	return s.store.ListBlacklists(ctx, custID, offset, limit)
}
```

- [ ] **Step 4: 实现 api（handlers + router）**

`templates/bank/internal/risk/api/handlers.go`:
```go
// Package api 是 risk 服务的传输层：http handlers + chi router。
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/risk/service"

	"github.com/go-chi/chi/v5"
)

// Handlers 持有 risk 只读服务。
type Handlers struct {
	Svc *service.RiskService
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListEvents 按条件筛选并分页（query: from/to/rule_id/action/offset/limit）。
func (h *Handlers) ListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListEvents(r.Context(), q.Get("from"), q.Get("to"), q.Get("rule_id"), q.Get("action"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]eventResp, 0, len(list))
	for _, e := range list {
		out = append(out, eventResp{
			EventID: e.EventID, BizDate: e.BizDate, CustID: e.CustID, RuleID: e.RuleID,
			RiskScore: e.RiskScore, ActionTaken: e.ActionTaken, Summary: e.Summary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// GetEvent 查事件详情（跨库联邦 JOIN）。不存在返回 404。
func (h *Handlers) GetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "event_id")
	d, err := h.Svc.Event(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("风控事件不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, eventDetailResp{
		eventResp: eventResp{
			EventID: d.EventID, BizDate: d.BizDate, CustID: d.CustID, RuleID: d.RuleID,
			RiskScore: d.RiskScore, ActionTaken: d.ActionTaken, Summary: d.Summary,
		},
		CustName: d.CustName, CustType: d.CustType,
	})
}

// ListRules 列风控规则（静态）。
func (h *Handlers) ListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.Svc.Rules(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// ListBlacklists 按客户筛选黑名单（query: cust_id/offset/limit）。
func (h *Handlers) ListBlacklists(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.Blacklists(r.Context(), q.Get("cust_id"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blacklists": list})
}

// --- DTO ---

type eventResp struct {
	EventID     string `json:"event_id"`
	BizDate     string `json:"biz_date"`
	CustID      string `json:"cust_id,omitempty"`
	RuleID      string `json:"rule_id,omitempty"`
	RiskScore   string `json:"risk_score,omitempty"`
	ActionTaken string `json:"action_taken,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type eventDetailResp struct {
	eventResp
	CustName string `json:"cust_name,omitempty"`
	CustType string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
```

`templates/bank/internal/risk/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 risk 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/risk/events", h.ListEvents)
		r.Get("/risk/events/{event_id}", h.GetEvent)
		r.Get("/risk/rules", h.ListRules)
		r.Get("/risk/blacklists", h.ListBlacklists)
	})
	return r
}
```

- [ ] **Step 5: 实现 cmd/risk/main.go**（仿 customer/main.go，库名 risk_db、端口 8084）

`templates/bank/cmd/risk/main.go`:
```go
// Package main 是 risk 只读 API 服务入口。
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
	"bank/internal/risk/api"
	"bank/internal/risk/repo"
	"bank/internal/risk/service"
)

func main() {
	dbName := getenv("DB_NAME", "risk_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Svc: service.NewRiskService(repo.NewRiskRepo(db)),
	}
	port := getenv("API_PORT", "8084")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("risk 监听 :%s (db=%s)", port, dbName)
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

Run:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/risk/...
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./cmd/risk
```
Expected: 测试 PASS；build 成功。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/risk/service/ templates/bank/internal/risk/api/ templates/bank/cmd/risk/
git commit -m "feat(bank): risk service + api + cmd (read-only, :8084)"
```

---

### Task 11: seed 编排接入 reward + risk（两 Track 汇合）

**Files:**
- Modify: `templates/bank/cmd/seed/main.go`
- Modify: `templates/bank/cmd/seed/seed_test.go`

**Interfaces:**
- Consumes: `GenRewardStatic/WriteRewardStatic/RunReward`（Task 4）、`GenRiskStatic/WriteRiskStatic/RunRisk`（Task 8）；customer 步的 `customers`、core 步的 `demandNos`。

- [ ] **Step 1: main.go 扩展 allDBs 与 runSeed**

把 `cmd/seed/main.go` 的 `allDBs` 改为：
```go
var allDBs = []struct{ name, sql string }{
	{"core_db", "db/migrations/core_db.sql"},
	{"cust_db", "db/migrations/cust_db.sql"},
	{"pay_db", "db/migrations/pay_db.sql"},
	{"reward_db", "db/migrations/reward_db.sql"},
	{"risk_db", "db/migrations/risk_db.sql"},
}
```

把 `main` 末尾的完成日志改为：
```go
	log.Println("[seed] 完成 ✅（5 库 + core + customer + payment + reward + risk + FDW）")
```

把 `runSeed` 里所有 `N/6` 步号改为 `N/8`，并在 payment 步（原 5/6）之后、setup_fdw（原 6/6）之前，插入 reward/risk 两步。修改后的相关段落为：

把 customer 步改为同时产出 `custIDs`（在生成 customers 后追加）：
```go
	customers := domains.GenCustomers(cfg, nCustomers)
	custIDs := make([]string, len(customers))
	for i, c := range customers {
		custIDs[i] = c.CustID
	}
```

payment 步后（`payDB.Close()` 之后），插入：
```go
	log.Println("[seed] 6/8 reward")
	rewardStatic := domains.GenRewardStatic(cfg, custIDs)
	campaignIDs := make([]string, len(rewardStatic.Campaigns))
	for i, c := range rewardStatic.Campaigns {
		campaignIDs[i] = c.CampaignID
	}
	rewardDB, err := pg.Open("reward_db")
	if err != nil {
		return err
	}
	if err := domains.WriteRewardStatic(ctx, rewardDB, rewardStatic); err != nil {
		rewardDB.Close()
		return err
	}
	if err := domains.RunReward(ctx, rewardDB, cfg, rewardStatic.PointsAccts, campaignIDs); err != nil {
		rewardDB.Close()
		return err
	}
	rewardDB.Close()

	log.Println("[seed] 7/8 risk")
	riskStatic := domains.GenRiskStatic(cfg, custIDs)
	riskDB, err := pg.Open("risk_db")
	if err != nil {
		return err
	}
	if err := domains.WriteRiskStatic(ctx, riskDB, riskStatic); err != nil {
		riskDB.Close()
		return err
	}
	if err := domains.RunRisk(ctx, riskDB, cfg, custIDs, demandNos); err != nil {
		riskDB.Close()
		return err
	}
	riskDB.Close()

	log.Println("[seed] 8/8 setup_fdw")
	if err := fdw.SetupFDW(ctx); err != nil {
		return fmt.Errorf("setup_fdw: %w", err)
	}
	return nil
```

> `demandNos` 在 core 步已生成；`customers`/`custIDs` 在 customer 步生成。其余步骤（core/customer/payment）不动，仅 `N/6`→`N/8` 改步号文案。

- [ ] **Step 2: seed_test.go 扩展（5 库 + reward/risk 灌数据 + 周末<工作日）**

把 `TestEnsureDBs_CreatesAllThree` 的库名列表与断言改为 5 库（并重命名函数为 `TestEnsureDBs_CreatesAllFive`，或保留函数名仅改内部——本步选改内部、保留函数名以免别处引用）：
```go
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db"}); err != nil {
		t.Fatalf("ensureDBs 失败: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db"} {
```

在 `TestSeedRun_PopulatesAllDBs` 的 `for _, c := range []struct{...}` 列表里追加 reward/risk 表断言：
```go
		{"reward_db", "points_acct"}, {"reward_db", "points_txn"}, {"reward_db", "coupon"},
		{"risk_db", "risk_rule"}, {"risk_db", "risk_event"}, {"risk_db", "blacklist"},
```

在该测试末尾（B-2 周末/工作日断言之后）追加 reward/risk 三因子断言：
```go
	// B-4a: reward/risk 逐日三因子——周末日均 < 工作日日均
	rewardDB, err := pg.Open("reward_db")
	if err != nil {
		t.Fatal(err)
	}
	var rwWk, rwWd float64
	if err := rewardDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM points_txn GROUP BY biz_date) q`).Scan(&rwWk, &rwWd); err != nil {
		t.Fatalf("查 reward 周末/工作日均值: %v", err)
	}
	if rwWk == 0 || rwWk >= rwWd {
		t.Errorf("reward 周末日均(%.0f) 应 < 工作日(%.0f)", rwWk, rwWd)
	}
	rewardDB.Close()
	// reward/risk 联邦外部表可查
	riskDB, err := pg.Open("risk_db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := riskDB.ExecContext(ctx, "SELECT cust_id FROM ext_cust_db_cust_info LIMIT 1"); err != nil {
		t.Errorf("risk_db 联邦表 ext_cust_db_cust_info 不可查: %v", err)
	}
	riskDB.Close()
```

- [ ] **Step 3: 编译 + 纯单测**

Run（`templates/bank/`）: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && go test ./...`
Expected: build OK；纯单测全绿。

- [ ] **Step 4: 集成测试（需 pg：先 `make up`）**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/ -run TestSeedRun -v
```
Expected: PASS（5 库灌数据 + reward 周末<工作日 + risk 联邦表可查）。注意：完整 seed 含 408 天逐日生成，可能耗时数十秒。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/cmd/seed/main.go templates/bank/cmd/seed/seed_test.go
git commit -m "feat(bank): B-4a seed 接入 reward + risk（8 步编排，逐日三因子）"
```

---

### Task 12: template.yaml + docker-compose + 重打包 + 全量验证

**Files:**
- Modify: `templates/bank/template.yaml`
- Modify: `templates/bank/docker-compose.yaml`
- Repack: jiade 根 `internal/template/templates.tar`

**Interfaces:** 无（收尾 + 验证）。

- [ ] **Step 1: template.yaml 加 2 db + 2 svc，version 0.3.0**

把 `template.yaml` 改为：
```yaml
name: bank
description: 简化版银行核心系统（core/customer/payment/reward/risk 服务，Spec B-4a）
version: 0.3.0
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
services:
  - {name: core-banking, port: 8080, db: core_db}
  - {name: customer, port: 8081, db: cust_db}
  - {name: payment, port: 8082, db: pay_db}
  - {name: reward, port: 8083, db: reward_db}
  - {name: risk, port: 8084, db: risk_db}
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

- [ ] **Step 2: docker-compose.yaml 加 reward + risk 两 service**

在 `payment` service 定义之后（`volumes:` 之前）追加：
```yaml
  reward:
    build:
      context: .
      args:
        CMD: reward
    container_name: bank-reward
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: reward_db
      API_PORT: "8083"
    ports: ["8083:8083"]
    depends_on:
      postgres: {condition: service_healthy}
  risk:
    build:
      context: .
      args:
        CMD: risk
    container_name: bank-risk
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: risk_db
      API_PORT: "8084"
    ports: ["8084:8084"]
    depends_on:
      postgres: {condition: service_healthy}
```

- [ ] **Step 3: bank module 全量（需 pg 跑集成）**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && go test ./... && go test -tags=integration ./...
```
Expected: build + 纯单测 + 集成全绿。

- [ ] **Step 4: 重打包 templates.tar**

Run（jiade 根）:
```
go generate ./internal/template
```
Expected: `internal/template/templates.tar` 更新。

- [ ] **Step 5: jiade 自身验证**

Run（jiade 根）:
```
go build ./... && go test ./...
```
Expected: 全绿（templates/bank 不参与 jiade build，generate 已重打包）。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/template.yaml templates/bank/docker-compose.yaml internal/template/templates.tar
git commit -m "feat(bank): B-4a template.yaml + compose 加 reward/risk（:8083/:8084），重打包"
```

---

## Self-Review（写后自查，已执行）

- **Spec coverage**：spec §5 schema → T1；§6.2 ScaleFactor / §6 rng / §7 FDW Mappings → T2；§8.2 reward Money + §reward 模型 → T3；§6.3 reward 静态+逐日 → T4；§7.2 reward 联邦端点 + §9 reward API → T5/T6；§8.3 risk 无 Money + risk 模型 → T7；§6.4 risk 静态+逐日 → T8；§7.2 risk 联邦 + §9 risk API → T9/T10；§10 seed 编排 → T11；§11 模板契约 + 重打包 → T12。验收 #1-#7 → T1-T12 + 集成断言。`coupon_usage` 有表无数据无端点（spec §5.3，正确无任务）。
- **Placeholder scan**：无 TBD/TODO；每步含完整代码或确切命令；rng 偏移（reward +30/+31、risk +32/+33）、端口（8083/8084）、FDW 映射均固化。
- **Type consistency**：`GenRewardStatic(cfg,custIDs) RewardStatic` 跨 T4/T11 一致；`RunReward(ctx,db,cfg,accts,campaignIDs)` 跨 T4/T11 一致；`GenRiskStatic(cfg,custIDs) RiskStatic` + `RunRisk(ctx,db,cfg,custIDs,accountNos)` 跨 T8/T11 一致；reward/risk Store 接口与 repo/fake 实现一一对应；`RiskEventDetail` 内嵌 `RiskEvent` 在 T7 定义、T9/T10 使用一致；`pickStr`/`maxInt`/`minInt` 在 `domains` 包内定义（reward.go），risk.go 同包复用（无重定义）。
- **deviation 已注明**：rng 偏移按 Jiade 已用值重分配（不沿用原型的 +20/+21，避 payment 碰撞）；ID 用确定 `RW-/RS-<date>-<seq>` 非 uuid4（原型 uuid4 非确定，Jiade 保确定性原则）；risk_score 经 `Float64()` 生成 2 位小数字符串（不进 domain/金额）。
