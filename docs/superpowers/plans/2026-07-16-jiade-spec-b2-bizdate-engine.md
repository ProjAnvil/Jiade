# B-2 多日切日引擎 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 Spec A 一次性快照式 core fixture 换成逐日滚存引擎（408 日历日 + 三因子 + 全量逐日余额快照 + 切 sys_param.biz_date），并让 B-3 记账 biz_date 读 sys_param。

**Architecture:** 方案 3——纯生成内核（distribution 三因子 + `GenDay`/`DayState`，无 DB，可确定性单测）+ 薄写入循环（`RunBizDate`：逐日批量 DELETE+INSERT + 末尾 UPSERT sys_param）。引擎落 `internal/fixtures/domains/bizdate.go`（package domains，与 core.go 同包）。B-3 衔接：`LedgerStore` 加 `GetBizDate`，`txn_service` 的 `today()` 改读它。

**Tech Stack:** Go 1.22 · database/sql · math/rand/v2 PCG · postgres（集成测试 `//go:build integration`）。

## Global Constraints

- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`）；改后须在 **jiade 根** 跑 `go generate ./internal/template` 重新打包 `templates.tar`（Task 6）。
- **go 1.22**：bank module `go.mod` pin 1.22；本地验证 macOS 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。
- **金额 int64 分，禁 float**：流水金额/余额全程 `domain.Money`。三因子 factor 是 float（仅缩放 int 交易量上下界，非金额）。
- **依赖方向向内**：`fixtures` 可 import `domain`；`domain` 零依赖。
- **删除需确认**：Task 4 删除 `GenBalanceRows/GenTxnRows/WriteBalances/WriteTxns/recentDates` 经 B-2 设计（替换快照）授权，执行时直接删。
- **确定性**：每日独立 rng（`seed+偏移+ordinal`）+ 确定 txn_id（`T<dateCompact>-<seq:05d>`，**非** crypto-rand）。

## File Structure

- **Create** `templates/bank/internal/fixtures/domains/bizdate.go` — 三因子 + DayState + GenDay(纯) + RunBizDate + 批量 writer（package domains）
- **Create** `templates/bank/internal/fixtures/domains/bizdate_test.go` — 纯单测（因子 + GenDay 确定性 + DayState 滚存）
- **Create** `templates/bank/internal/fixtures/domains/bizdate_integration_test.go` — `//go:build integration`，RunBizDate 真 pg
- **Modify** `templates/bank/cmd/seed/main.go` — step3 core 用 `RunBizDate` 替换 `WriteBalances/WriteTxns`
- **Modify** `templates/bank/internal/fixtures/domains/core.go` — 删除 GenBalanceRows/GenTxnRows/WriteBalances/WriteTxns/recentDates（保留 nullable、addMonths）
- **Modify** `templates/bank/internal/fixtures/domains/core_test.go` — 删除 TestGenBalanceRows_Deterministic / TestGenTxnRows_Deterministic
- **Modify** `templates/bank/cmd/seed/seed_test.go` — 扩展：断言多日数据 + sys_param + 周末/季末因子（聚合）
- **Modify** `templates/bank/internal/corebanking/repo/ledger_repo.go` — +`GetBizDate`
- **Modify** `templates/bank/internal/corebanking/service/ledger_service.go` — LedgerStore 接口 +`GetBizDate`
- **Modify** `templates/bank/internal/corebanking/service/txn_service.go` — `today()`→`GetBizDate`，删 `today()`+`time` import
- **Modify** `templates/bank/internal/corebanking/service/ledger_service_test.go` — recordingLedgerStore +`GetBizDate`
- **Modify** `templates/bank/internal/corebanking/service/txn_service_test.go` — +正向测试 BizDate 取自 sys_param
- **Modify** `templates/bank/internal/corebanking/api/handlers_test.go` — recordingAPIStore +`GetBizDate`
- **Repack** `templates.tar`（jiade 根 `go generate`）— Task 6

## Execution Tracks（fan-out 用）

- **Track A（引擎，顺序）**：Task 1 → 2 → 3 → 4。文件域：`fixtures/domains/bizdate*.go`、`core.go`、`core_test.go`、`cmd/seed/*`。
- **Track B（B-3 衔接）**：Task 5。文件域：`corebanking/{repo,service,api}/*`。
- 两 Track **文件不相交**，可并行。Task 6（重打包 + 全量验证）依赖两 Track 完成。

---

### Task 1: distribution 三因子 + 日期 helper（纯）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/bizdate.go`
- Test: `templates/bank/internal/fixtures/domains/bizdate_test.go`

**Interfaces:**
- Produces: `trendFactor(d time.Time) float64`、`seasonalFactor(d time.Time) float64`、`cyclicalFactor(d time.Time) float64`、`volumeForDay(cfg fixtures.Config, d time.Time) int`、`parseDate(s string) time.Time`、`dayOrdinal(d, base time.Time) int64`、`dateCompact(d time.Time) string`、`bizDateRange(start, end string) ([]time.Time, error)`、`isHoliday(d time.Time) bool`、`var holidays map[string]bool`。Task 2/3 复用这些。

- [ ] **Step 1: 写 bizdate.go（因子 + helper 部分）**

```go
// Package domains 的 bizdate 子模块：多日切日引擎（移植 bossy bizdate+distribution）。
package domains

import (
	"strconv"
	"strings"
	"time"

	"bank/internal/fixtures"
)

// bossy 节假日（公历固定，移植自 distribution.py HOLIDAYS）。
var holidays = map[string]bool{
	"2025-10-01": true, "2025-10-02": true, "2025-10-03": true, // 国庆
	"2026-01-01": true,                                         // 元旦
	"2026-02-16": true, "2026-02-17": true, "2026-02-18": true, // 春节
	"2025-10-10": true, "2025-11-11": true,                     // 双十一
}

func isHoliday(d time.Time) bool { return holidays[d.Format("2006-01-02")] }

// trendFactor 每月 +2%（base 2025-06-01，移植 bossy trend_factor）。
func trendFactor(d time.Time) float64 {
	const baseYear, baseMonth = 2025, 6
	months := (d.Year()-baseYear)*12 + int(d.Month()) - baseMonth
	return 1.0 + 0.02*float64(months)
}

// seasonalFactor 季末×1.35 / 年末×1.50 / 发薪日×1.40 / 节假日×1.30（乘性叠加）。
func seasonalFactor(d time.Time) float64 {
	f := 1.0
	m := int(d.Month())
	if (m == 3 || m == 6 || m == 9 || m == 12) && d.Day() >= 28 { // 季末考核
		f *= 1.35
	}
	if m == 12 && d.Day() >= 25 { // 年末 surge
		f *= 1.50
	}
	if d.Day() == 10 || d.Day() == 15 { // 发薪日
		f *= 1.40
	}
	if isHoliday(d) { // 节假日消费高峰
		f *= 1.30
	}
	return f
}

// cyclicalFactor 周末 ~60%（移植 bossy cyclical_factor）。
func cyclicalFactor(d time.Time) float64 {
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		return 0.60
	}
	return 1.0
}

// volumeForDay 当日交易笔数。每日独立 rng（seed+100+ordinal），factor=trend×seasonal×cyclical。
// 单日结果只依赖日期，子范围重跑可复现（对齐 bossy volume_fn）。
func volumeForDay(cfg fixtures.Config, d time.Time) int {
	base := parseDate(cfg.StartBizDate)
	rng := fixtures.NewRNG(cfg.Seed + 100 + dayOrdinal(d, base))
	factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
	tc := cfg.TargetCounts()
	lo := int(float64(tc.DailyTxnLo) * factor)
	if lo < 1 {
		lo = 1
	}
	hi := int(float64(tc.DailyTxnHi) * factor)
	if hi < lo+1 {
		hi = lo + 1
	}
	return rng.IntRange(lo, hi)
}

// ---- 日期 helper ----

func parseDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// dayOrdinal d 自 base 起的天数（含 0）。
func dayOrdinal(d, base time.Time) int64 {
	return int64(d.Sub(base).Hours() / 24)
}

// dateCompact "2026-07-16" → "20260716"（txn_id 用）。
func dateCompact(d time.Time) string {
	var b strings.Builder
	for _, c := range d.Format("2006-01-02") {
		if c != '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// bizDateRange [start,end]（YYYY-MM-DD，含两端）的日历日列表。
func bizDateRange(start, end string) ([]time.Time, error) {
	t0, t1 := parseDate(start), parseDate(end)
	if t0.IsZero() || t1.IsZero() {
		return nil, fmt.Errorf("bizdate: 非法日期 %s~%s", start, end)
	}
	if t1.Before(t0) {
		return nil, fmt.Errorf("bizdate: end<start %s~%s", start, end)
	}
	var out []time.Time
	for d := t0; !d.After(t1); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out, nil
}

// 占位：避免 strconv 未用 import 报错（Task 3 的 placeholders 会用到 strconv）。
var _ = strconv.Itoa
```

> 注意：末行 `var _ = strconv.Itoa` 是临时占位防止 import 报错；Task 3 加 placeholders 后删除该行。

- [ ] **Step 2: 写 bizdate_test.go（因子单测）**

```go
package domains

import (
	"testing"
	"time"

	"bank/internal/fixtures"
)

func TestFactor_Trend(t *testing.T) {
	base := parseDate("2025-06-01")
	if trendFactor(base) != 1.0 {
		t.Errorf("base 月因子应 1.0, got %v", trendFactor(base))
	}
	// 3 个月后：1.0+0.02*3=1.06
	if got := trendFactor(parseDate("2025-09-15")); got != 1.06 {
		t.Errorf("9 月因子应 1.06, got %v", got)
	}
}

func TestFactor_Seasonal(t *testing.T) {
	// 普通日
	if got := seasonalFactor(parseDate("2025-06-12")); got != 1.0 {
		t.Errorf("普通日应 1.0, got %v", got)
	}
	// 季末日（6 月 28）×1.35
	if got := seasonalFactor(parseDate("2025-06-28")); got != 1.35 {
		t.Errorf("季末日应 1.35, got %v", got)
	}
	// 发薪日 ×1.40
	if got := seasonalFactor(parseDate("2025-07-10")); got != 1.40 {
		t.Errorf("发薪日应 1.40, got %v", got)
	}
	// 节假日（国庆）×1.30
	if got := seasonalFactor(parseDate("2025-10-01")); got != 1.30 {
		t.Errorf("国庆应 1.30, got %v", got)
	}
	// 年末 12-25 ×1.50×1.35（季末+年末）
	if got := seasonalFactor(parseDate("2025-12-25")); got != 1.50*1.35 {
		t.Errorf("年末 12-25 应 %v, got %v", 1.50*1.35, got)
	}
}

func TestFactor_Cyclical(t *testing.T) {
	// 2025-06-01 是周日
	if got := cyclicalFactor(parseDate("2025-06-01")); got != 0.60 {
		t.Errorf("周日应 0.60, got %v", got)
	}
	// 2025-06-02 周一
	if got := cyclicalFactor(parseDate("2025-06-02")); got != 1.0 {
		t.Errorf("工作日应 1.0, got %v", got)
	}
}

func TestVolumeForDay_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	d := parseDate("2025-07-15")
	a, b := volumeForDay(cfg, d), volumeForDay(cfg, d)
	if a != b {
		t.Fatalf("volumeForDay 不确定: %d != %d", a, b)
	}
	if a < 1 {
		t.Errorf("volume 应 ≥1, got %d", a)
	}
}

func TestVolumeForDay_WeekendDampened(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	// 同一周：周三 vs 周日（cyclical 压周末，量级上界应周日更小）
	wd := volumeForDay(cfg, parseDate("2025-07-16")) // 周三
	wk := volumeForDay(cfg, parseDate("2025-07-20")) // 周日
	if wk >= wd {
		t.Errorf("周末 volume(%d) 应 < 工作日(%d)", wk, wd)
	}
}

func TestDateHelpers(t *testing.T) {
	if dateCompact(parseDate("2026-07-16")) != "20260716" {
		t.Error("dateCompact 错")
	}
	days, err := bizDateRange("2025-06-01", "2025-06-03")
	if err != nil || len(days) != 3 {
		t.Errorf("bizDateRange 应 3 天, got %d err=%v", len(days), err)
	}
	if dayOrdinal(parseDate("2025-06-03"), parseDate("2025-06-01")) != 2 {
		t.Error("dayOrdinal 错")
	}
}

func init() { _ = time.Now } // 保持 time import（如需要）
```

- [ ] **Step 3: 跑测试**

Run: `cd templates/bank && go test ./internal/fixtures/domains/ -run 'TestFactor|TestVolumeForDay|TestDateHelpers' -v`
Expected: PASS（全部绿）。

- [ ] **Step 4: Commit**

```bash
git add templates/bank/internal/fixtures/domains/bizdate.go templates/bank/internal/fixtures/domains/bizdate_test.go
git commit -m "feat(bank): B-2 distribution 三因子 + 日期 helper（纯）"
```

---

### Task 2: GenDay 内核 + DayState（纯）

**Files:**
- Modify: `templates/bank/internal/fixtures/domains/bizdate.go`（追加 GenDay/DayState/newDayState + imports）
- Test: `templates/bank/internal/fixtures/domains/bizdate_test.go`（追加测试）

**Interfaces:**
- Consumes: Task 1 的 `volumeForDay`、`parseDate`、`dayOrdinal`、`dateCompact`。
- Produces: `type DayState struct{ Bal map[string]domain.Money }`、`func newDayState(cfg, demandNos) *DayState`、`func GenDay(cfg, date time.Time, demandNos []string, st *DayState) (dayTxns []domain.Txn, dayBalances []domain.Balance)`。Task 3 的 RunBizDate 复用。

- [ ] **Step 1: bizdate.go 加 imports**

把 bizdate.go 的 import 块改为：

```go
import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
)
```

- [ ] **Step 2: bizdate.go 追加引擎内核（文件末尾）**

```go
// ---- 引擎内核（纯函数，无 DB）----

// DayState 账户余额的内存滚存态（对齐 bossy DailyState）。
type DayState struct{ Bal map[string]domain.Money }

// newDayState 初始化每账户余额（rng seed+2，回收 Spec A 已删 GenBalanceRows 的偏移）。
func newDayState(cfg fixtures.Config, demandNos []string) *DayState {
	rng := fixtures.NewRNG(cfg.Seed + 2)
	st := &DayState{Bal: make(map[string]domain.Money, len(demandNos))}
	for _, no := range demandNos {
		st.Bal[no] = domain.NewMoneyFromCents(int64(rng.IntRange(1, 99999)) * 10000)
	}
	return st
}

// GenDay 生成当日流水 + 当日全账户余额快照，推进 st。纯函数。
// 每日独立内容 rng（seed+200+ordinal）；流水贷多借少→余额温和增长；txn_id 确定。
func GenDay(cfg fixtures.Config, date time.Time, demandNos []string, st *DayState) (dayTxns []domain.Txn, dayBalances []domain.Balance) {
	if len(demandNos) == 0 {
		return nil, nil
	}
	n := volumeForDay(cfg, date)
	rng := fixtures.NewRNG(cfg.Seed + 200 + dayOrdinal(date, parseDate(cfg.StartBizDate)))
	dateStr := date.Format("2006-01-02")
	compact := dateCompact(date)

	dayTxns = make([]domain.Txn, 0, n)
	for i := 0; i < n; i++ {
		acct := demandNos[rng.IntRange(0, len(demandNos)-1)]
		amt := domain.NewMoneyFromCents(int64(rng.IntRange(1, 9999)) * 1000)
		credit := rng.IntRange(0, 2) < 2 // 0/1=贷(2/3)，2=借(1/3)
		var dc domain.DCFlag
		if credit {
			dc = domain.DCCredit
			st.Bal[acct] = st.Bal[acct].Add(amt)
		} else {
			dc = domain.DCDebit
			b := st.Bal[acct].Sub(amt)
			if b < 0 {
				b = domain.NewMoneyFromCents(0) // clamp 0（对齐 bossy max(0,...)）
			}
			st.Bal[acct] = b
		}
		dayTxns = append(dayTxns, domain.Txn{
			TxnID:       fmt.Sprintf("T%s-%05d", compact, i),
			BizDate:     dateStr,
			AccountNo:   acct,
			DCFlag:      dc,
			Amount:      amt,
			Ccy:         "CNY",
			SubjectCode: "2011",
			OppAccount:  demandNos[rng.IntRange(0, len(demandNos)-1)],
			Channel:     rng.Choice(fixtures.Channels),
			Summary:     rng.Choice(fixtures.Summaries),
			VoucherNo:   "",
			TxnStatus:   domain.TxnStatusNormal,
		})
	}

	dayBalances = make([]domain.Balance, 0, len(demandNos))
	for _, no := range demandNos { // 按 demandNos 顺序快照（确定性，不 range map）
		bal := st.Bal[no]
		dayBalances = append(dayBalances, domain.Balance{
			AccountNo: no, BizDate: dateStr,
			Balance: bal, AvailableBalance: bal,
			FrozenAmount: domain.NewMoneyFromCents(0), SubjectCode: "2011",
		})
	}
	return dayTxns, dayBalances
}
```

- [ ] **Step 3: bizdate_test.go 追加测试**

```go
func TestGenDay_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	nos := []string{"D0000000001", "D0000000002", "D0000000003"}
	date := parseDate("2025-07-15")
	st1, st2 := newDayState(cfg, nos), newDayState(cfg, nos)
	t1, b1 := GenDay(cfg, date, nos, st1)
	t2, b2 := GenDay(cfg, date, nos, st2)
	if !reflect.DeepEqual(t1, t2) || !reflect.DeepEqual(b1, b2) {
		t.Fatal("同输入两次 GenDay 不一致")
	}
	if len(t1) == 0 {
		t.Error("应生成流水")
	}
}

func TestGenDay_TxnIDUniqueAndFormatted(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	nos := []string{"D0000000001", "D0000000002"}
	txns, _ := GenDay(cfg, parseDate("2025-07-15"), nos, newDayState(cfg, nos))
	seen := map[string]bool{}
	for _, tx := range txns {
		if tx.TxnID == "" || !strings.HasPrefix(tx.TxnID, "T20250715-") {
			t.Errorf("txn_id 格式错: %q", tx.TxnID)
		}
		if seen[tx.TxnID] {
			t.Errorf("txn_id 重复: %q", tx.TxnID)
		}
		seen[tx.TxnID] = true
	}
}

func TestGenDay_DcWeighting(t *testing.T) {
	// 多账户 + 多日累计，贷:借 应近似 2:1（容差大）
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	nos := make([]string, 50)
	for i := range nos {
		nos[i] = fmt.Sprintf("D%010d", i+1)
	}
	st := newDayState(cfg, nos)
	credit, debit := 0, 0
	for _, d := range []string{"2025-07-10", "2025-07-11", "2025-07-12"} {
		txns, _ := GenDay(cfg, parseDate(d), nos, st)
		for _, tx := range txns {
			if tx.DCFlag == domain.DCCredit {
				credit++
			} else {
				debit++
			}
		}
	}
	if debit == 0 || credit/debit < 1 || credit/debit > 3 {
		t.Errorf("贷:借 = %d:%d，期望近似 2:1", credit, debit)
	}
}

func TestDayState_RollForward(t *testing.T) {
	// 脚本化：贷加、借 clamp 0
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	nos := []string{"D1"}
	st := &DayState{Bal: map[string]domain.Money{"D1": domain.NewMoneyFromCents(50000)}} // 500.00
	// 手动模拟：GenDay 内部对 st.Bal 的推进规则
	st.Bal["D1"] = st.Bal["D1"].Add(domain.NewMoneyFromCents(30000)) // +300 → 800
	if got := st.Bal["D1"].Cents(); got != 80000 {
		t.Fatalf("贷后余额错: %d", got)
	}
	b := st.Bal["D1"].Sub(domain.NewMoneyFromCents(200000)) // -2000 → 负
	if b < 0 {
		b = domain.NewMoneyFromCents(0)
	}
	st.Bal["D1"] = b
	if got := st.Bal["D1"].Cents(); got != 0 {
		t.Fatalf("借 clamp 0 错: %d", got)
	}
	// GenDay 余额快照覆盖全部账户
	_, bals := GenDay(cfg, parseDate("2025-07-15"), nos, newDayState(cfg, nos))
	if len(bals) != 1 || bals[0].AccountNo != "D1" {
		t.Errorf("快照应覆盖全部账户, got %+v", bals)
	}
	if bals[0].AvailableBalance != bals[0].Balance || bals[0].FrozenAmount.Cents() != 0 {
		t.Errorf("available/frozen 语义错: %+v", bals[0])
	}
}
```

> 测试文件顶部 import 补 `"fmt"`、`"reflect"`、`"strings"`、`"bank/internal/corebanking/domain"`（按需）。删除 Task 1 占位的 `func init()`（若已无 time 引用则去掉 time import）。

- [ ] **Step 4: 跑测试**

Run: `cd templates/bank && go test ./internal/fixtures/domains/ -run 'TestGenDay|TestDayState' -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/fixtures/domains/bizdate.go templates/bank/internal/fixtures/domains/bizdate_test.go
git commit -m "feat(bank): B-2 GenDay 内核 + DayState 滚存（纯）"
```

---

### Task 3: RunBizDate 写入循环 + 批量 writer（集成）

**Files:**
- Modify: `templates/bank/internal/fixtures/domains/bizdate.go`（追加 RunBizDate + bulkInsert + placeholders）
- Create: `templates/bank/internal/fixtures/domains/bizdate_integration_test.go`

**Interfaces:**
- Consumes: Task 1/2 的 `bizDateRange`、`GenDay`、`newDayState`；`pg.RunInTx`/`pg.DBTX`；core.go 的 `nullable`（package domains 共享）。
- Produces: `func RunBizDate(ctx, db, cfg, demandNos) error`、`func bulkInsertTxns`、`func bulkInsertBalances`、`func placeholders(nRows, nCols int) string`。Task 4 的 seed 调用 `RunBizDate`。

- [ ] **Step 1: bizdate.go 加 imports + 删占位**

import 块改为：

```go
import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
	"bank/internal/platform/pg"
)
```

删除 Task 1 的占位行 `var _ = strconv.Itoa`。

- [ ] **Step 2: bizdate.go 追加写入循环（文件末尾）**

```go
// ---- 写入循环（批量 + 逐日幂等 + 切 sys_param）----

const bizDateBatchSize = 1000

// RunBizDate 从 StartBizDate 推进到 EndBizDate：每日 GenDay → 逐日 tx 内 DELETE 当日 + 批量 INSERT；
// 末尾 UPSERT sys_param.biz_date（=EndBizDate）/prev_biz_date（=次末日）。
func RunBizDate(ctx context.Context, db *sql.DB, cfg fixtures.Config, demandNos []string) error {
	if len(demandNos) == 0 {
		return fmt.Errorf("bizdate: 无账户")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("bizdate: %w", err)
	}
	st := newDayState(cfg, demandNos)
	for _, d := range days {
		dayTxns, dayBalances := GenDay(cfg, d, demandNos, st)
		dateStr := d.Format("2006-01-02")
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM acct_txn WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日流水 %s: %w", dateStr, err)
			}
			if err := bulkInsertTxns(ctx, q, dayTxns); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM account_balance WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日余额 %s: %w", dateStr, err)
			}
			if err := bulkInsertBalances(ctx, q, dayBalances); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("bizdate: 写 %s 失败: %w", dateStr, err)
		}
	}
	end := days[len(days)-1].Format("2006-01-02")
	prev := days[len(days)-1].AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := db.ExecContext(ctx, `INSERT INTO sys_param(param_key,param_value) VALUES ('biz_date',$1),('prev_biz_date',$2)
		ON CONFLICT (param_key) DO UPDATE SET param_value=EXCLUDED.param_value`, end, prev); err != nil {
		return fmt.Errorf("bizdate: 切 sys_param: %w", err)
	}
	return nil
}

// placeholders 生成 nRows×nCols 的 $N 占位符串：($1,$2),($3,$4),...
func placeholders(nRows, nCols int) string {
	var b strings.Builder
	idx := 1
	for r := 0; r < nRows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < nCols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteString("$")
			b.WriteString(strconv.Itoa(idx))
			idx++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// bulkInsertTxns 批量插 acct_txn（11 列；voucher_no/txn_status 走 DEFAULT ''/'normal'）。
func bulkInsertTxns(ctx context.Context, q pg.DBTX, rows []domain.Txn) error {
	const cols = 11
	const stmt = "INSERT INTO acct_txn(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, t := range chunk {
			args = append(args, t.TxnID, t.BizDate, t.AccountNo, string(t.DCFlag), t.Amount.String(),
				t.Ccy, t.SubjectCode, nullable(t.OppAccount), nullable(t.RefTxnID), nullable(t.Channel), nullable(t.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("bizdate: 批量插流水: %w", err)
		}
	}
	return nil
}

// bulkInsertBalances 批量插 account_balance（6 列）。
func bulkInsertBalances(ctx context.Context, q pg.DBTX, rows []domain.Balance) error {
	const cols = 6
	const stmt = "INSERT INTO account_balance(account_no,biz_date,balance,available_balance,frozen_amount,subject_code) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, b := range chunk {
			args = append(args, b.AccountNo, b.BizDate, b.Balance.String(), b.AvailableBalance.String(),
				b.FrozenAmount.String(), b.SubjectCode)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("bizdate: 批量插余额: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 3: 写 bizdate_integration_test.go**

```go
//go:build integration

package domains

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"bank/internal/fixtures"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
)

// setupCoreDB 重建 core_db 并建表（破坏性：DROP core_db）。无 pg 则 skip。
func setupCoreDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪（先 make up）: %v", err)
	}
	admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='core_db' AND pid<>pg_backend_pid()")
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "core_db"`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "core_db"`); err != nil {
		t.Fatal(err)
	}
	db, err := pg.Open("core_db")
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile("../../../db/migrations/core_db.sql") // domains → bank 根 3 级上溯
	if err != nil {
		t.Skipf("读 core_db.sql 失败（cwd?）: %v", err)
	}
	if err := migrate.Run(ctx, db, string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRunBizDate_WritesAllDaysAndCutsSysParam(t *testing.T) {
	ctx := context.Background()
	db := setupCoreDB(t)
	defer db.Close()
	cfg := fixtures.Config{StartBizDate: "2025-06-01", EndBizDate: "2025-06-30", Scale: fixtures.ScaleDev, Seed: 42}
	nos := make([]string, 20)
	for i := range nos {
		nos[i] = fmt.Sprintf("D%010d", i+1)
	}
	if err := RunBizDate(ctx, db, cfg, nos); err != nil {
		t.Fatalf("RunBizDate: %v", err)
	}
	var bd string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bd); err != nil {
		t.Fatalf("查 biz_date: %v", err)
	}
	if bd != "2025-06-30" {
		t.Errorf("sys_param.biz_date=%q want 2025-06-30", bd)
	}
	var days int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days != 30 {
		t.Errorf("acct_txn 覆盖天数=%d want 30", days)
	}
	var bal int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_balance").Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != 30*20 {
		t.Errorf("account_balance 行=%d want %d", bal, 30*20)
	}
	// 幂等：二次跑，天数不变（逐日 DELETE+INSERT）
	if err := RunBizDate(ctx, db, cfg, nos); err != nil {
		t.Fatalf("二次 RunBizDate: %v", err)
	}
	var days2 int
	db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days2)
	if days2 != 30 {
		t.Errorf("二次跑后天数=%d want 30（应幂等）", days2)
	}
}
```

- [ ] **Step 4: 跑纯单测（确认未破坏 Task 1/2）**

Run: `cd templates/bank && go test ./internal/fixtures/domains/ -v`
Expected: PASS（纯单测全绿；集成测试因 build tag 不参与）。

- [ ] **Step 5: 跑集成测试（需 pg：先 `make up` 或 `docker compose up -d postgres`）**

Run: `cd templates/bank && go test -tags=integration ./internal/fixtures/domains/ -run TestRunBizDate -v`
Expected: PASS（30 天、sys_param=2025-06-30、600 余额行、二次跑幂等）。

- [ ] **Step 6: Commit**

```bash
git add templates/bank/internal/fixtures/domains/bizdate.go templates/bank/internal/fixtures/domains/bizdate_integration_test.go
git commit -m "feat(bank): B-2 RunBizDate 写入循环 + 批量 writer + 集成测试"
```

---

### Task 4: seed 编排替换 + 删除旧快照（Track A 收尾）

**Files:**
- Modify: `templates/bank/cmd/seed/main.go:82-86`
- Modify: `templates/bank/internal/fixtures/domains/core.go`（删 5 个函数 + recentDates）
- Modify: `templates/bank/internal/fixtures/domains/core_test.go`（删 2 个测试）
- Modify: `templates/bank/cmd/seed/seed_test.go`（加 B-2 断言）

**Interfaces:**
- Consumes: Task 3 的 `RunBizDate`。core.go 的 `nullable`（bizdate.go 已用）。

- [ ] **Step 1: main.go step3 用 RunBizDate**

把 `cmd/seed/main.go` 的（约 82-86 行）：

```go
	if err := domains.WriteBalances(ctx, coreDB, domains.GenBalanceRows(cfg, demandNos)); err != nil {
		return err
	}
	if err := domains.WriteTxns(ctx, coreDB, domains.GenTxnRows(cfg, demandNos)); err != nil {
		return err
	}
```

替换为：

```go
	if err := domains.RunBizDate(ctx, coreDB, cfg, demandNos); err != nil {
		return err
	}
```

- [ ] **Step 2: 删 core.go 旧函数**

删除 `internal/fixtures/domains/core.go` 中的：`GenBalanceRows`（约 100-113）、`GenTxnRows`（约 115-145）、`WriteBalances`（约 221-236）、`WriteTxns`（约 238-253）、`recentDates`（约 265-275）。

**保留**：`nullable`（约 277，bizdate.go 的 bulkInsertTxns 复用）、`addMonths`（GenAccountRows 用）、`GenStaticData`/`GenAccountRows`/`WriteStatic`/`WriteAccounts`。

> 删除经 B-2 设计（替换快照）授权，直接删。

- [ ] **Step 3: 删 core_test.go 旧测试**

删除 `core_test.go` 中的 `TestGenBalanceRows_Deterministic`（约 22-31）与 `TestGenTxnRows_Deterministic`（约 33-51）。保留 `TestGenAccountRows_Deterministic`、`TestGenStaticData_FixedContent`。

- [ ] **Step 4: seed_test.go 加 B-2 断言**

在 `cmd/seed/seed_test.go` 的 `TestSeedRun_PopulatesAllDBs` 末尾（`fdw 外部表可查` 断言之后、函数结束前）加：

```go
	// B-2: core 多日切日引擎
	coreDB2, err := pg.Open("core_db")
	if err != nil {
		t.Fatal(err)
	}
	defer coreDB2.Close()
	var bizDate string
	if err := coreDB2.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bizDate); err != nil {
		t.Fatalf("查 sys_param.biz_date: %v", err)
	}
	if bizDate != "2026-07-13" {
		t.Errorf("sys_param.biz_date=%q want 2026-07-13", bizDate)
	}
	var txnDays int
	if err := coreDB2.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&txnDays); err != nil {
		t.Fatalf("查 acct_txn 天数: %v", err)
	}
	if txnDays < 400 {
		t.Errorf("acct_txn 覆盖天数=%d, want ≥400", txnDays)
	}
	// 周末日均 < 工作日日均（cyclical ×0.60，聚合稳健）
	var wkAvg, wdAvg float64
	err = coreDB2.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM acct_txn GROUP BY biz_date) q`).Scan(&wkAvg, &wdAvg)
	if err != nil {
		t.Fatalf("查周末/工作日均值: %v", err)
	}
	if wkAvg >= wdAvg {
		t.Errorf("周末日均(%.0f) 应 < 工作日日均(%.0f)", wkAvg, wdAvg)
	}
```

- [ ] **Step 5: 编译 + 纯单测**

Run: `cd templates/bank && go build ./... && go test ./...`
Expected: build OK；纯单测全绿（集成测试不参与）。

- [ ] **Step 6: 集成测试（需 pg）**

Run: `cd templates/bank && go test -tags=integration ./cmd/seed/ -run TestSeedRun -v`
Expected: PASS（含新 B-2 断言：biz_date=2026-07-13、≥400 天、周末<工作日）。

- [ ] **Step 7: Commit**

```bash
git add templates/bank/cmd/seed/main.go templates/bank/cmd/seed/seed_test.go templates/bank/internal/fixtures/domains/core.go templates/bank/internal/fixtures/domains/core_test.go
git commit -m "feat(bank): B-2 seed 接入 RunBizDate，删除 Spec A 快照生成"
```

---

### Task 5: B-3 衔接——记账 biz_date 读 sys_param.biz_date（Track B）

**Files:**
- Modify: `templates/bank/internal/corebanking/repo/ledger_repo.go`（+GetBizDate）
- Modify: `templates/bank/internal/corebanking/service/ledger_service.go`（LedgerStore 接口 +GetBizDate）
- Modify: `templates/bank/internal/corebanking/service/ledger_service_test.go`（recordingLedgerStore +GetBizDate）
- Modify: `templates/bank/internal/corebanking/api/handlers_test.go`（recordingAPIStore +GetBizDate）
- Modify: `templates/bank/internal/corebanking/service/txn_service.go`（today()→GetBizDate；删 today()+time import）
- Modify: `templates/bank/internal/corebanking/service/txn_service_test.go`（+正向测试）

**Interfaces:**
- Produces: `LedgerRepo.GetBizDate(ctx) (string, error)`、`LedgerStore.GetBizDate(ctx) (string, error)`。`txn_service.Record/Reverse` 改读它。

- [ ] **Step 1: repo 加 GetBizDate**

在 `ledger_repo.go` 末尾（`GetGL` 之后）加：

```go
// GetBizDate 读 sys_param.biz_date（B-2 衔接：记账 biz_date 取自 sys_param 而非 time.Now）。
func (r *LedgerRepo) GetBizDate(ctx context.Context) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&v)
	if err != nil {
		return "", fmt.Errorf("repo: 读 sys_param.biz_date: %w", err)
	}
	return v, nil
}
```

- [ ] **Step 2: LedgerStore 接口加 GetBizDate**

在 `ledger_service.go` 的 `LedgerStore` 接口内（`SetTxnSummary` 之后）加一行：

```go
	// GetBizDate 读 sys_param.biz_date（B-2：记账 biz_date 来源）。
	GetBizDate(ctx context.Context) (string, error)
```

- [ ] **Step 3: recordingLedgerStore 加 stub**

在 `ledger_service_test.go` 的 `recordingLedgerStore` 末尾（`SetTxnSummary` 方法之后）加：

```go
func (f *recordingLedgerStore) GetBizDate(context.Context) (string, error) {
	f.calls++
	return "2026-07-13", nil
}
```

- [ ] **Step 4: recordingAPIStore 加 stub**

在 `handlers_test.go` 的 `recordingAPIStore` 末尾（其 `SetTxnSummary` 方法之后）加：

```go
func (*recordingAPIStore) GetBizDate(context.Context) (string, error) { return "2026-07-13", nil }
```

- [ ] **Step 5: txn_service.Record 改读 GetBizDate**

`txn_service.go` `Record` 开头（`bizDate := today()` / `voucherNo := ...`）替换：

```go
	bizDate := today()
	voucherNo := domain.NewVoucherNo(bizDate)
```

为：

```go
	bizDate, err := s.store.GetBizDate(ctx)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("txn: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return domain.Booking{}, fmt.Errorf("txn: sys_param.biz_date 未设置，请先 seed")
	}
	voucherNo := domain.NewVoucherNo(bizDate)
```

- [ ] **Step 6: txn_service.Reverse 改读 GetBizDate**

`Reverse` 开头 `bizDate := today()` 替换为：

```go
	bizDate, err := s.store.GetBizDate(ctx)
	if err != nil {
		return ReverseResult{}, fmt.Errorf("txn: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return ReverseResult{}, fmt.Errorf("txn: sys_param.biz_date 未设置，请先 seed")
	}
```

- [ ] **Step 7: 删 today() + time import**

删除 `txn_service.go` 末尾的 `func today() string { return time.Now().Format("2006-01-02") }`，并从 import 块删除 `"time"`（如确认无其他引用）。

- [ ] **Step 8: 加正向测试**

在 `txn_service_test.go` 末尾加：

```go
func TestRecord_BizDateFromSysParam(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)
	booking, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("deposit 应成功: %v", err)
	}
	// biz_date 取自 fake store 的 GetBizDate（返回 2026-07-13），非 time.Now()
	if booking.BizDate != "2026-07-13" {
		t.Errorf("biz_date 应取自 sys_param, got %q want 2026-07-13", booking.BizDate)
	}
}
```

- [ ] **Step 9: 跑 service + api 单测**

Run: `cd templates/bank && go test ./internal/corebanking/... -v`
Expected: PASS（含新 `TestRecord_BizDateFromSysParam`；现有 B-3 测试不受影响——它们不断言 BizDate）。

- [ ] **Step 10: Commit**

```bash
git add templates/bank/internal/corebanking/repo/ledger_repo.go templates/bank/internal/corebanking/service/ledger_service.go templates/bank/internal/corebanking/service/txn_service.go templates/bank/internal/corebanking/service/ledger_service_test.go templates/bank/internal/corebanking/service/txn_service_test.go templates/bank/internal/corebanking/api/handlers_test.go
git commit -m "feat(bank): B-2 衔接 记账 biz_date 读 sys_param.biz_date（GetBizDate）"
```

---

### Task 6: 重打包 templates.tar + 全量验证（两 Track 汇合后）

**Files:**
- Repack: jiade 根 `go generate ./internal/template` → `templates.tar`

**Interfaces:** 无（验证性任务）。

- [ ] **Step 1: bank module 全量（需 pg 跑集成）**

Run:
```bash
cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && go test ./... && go test -tags=integration ./...
```
Expected: build + 纯单测 + 集成全绿。

- [ ] **Step 2: 重打包 templates.tar**

Run（jiade 根）:
```bash
go generate ./internal/template
```
Expected: `templates.tar` 更新。

- [ ] **Step 3: jiade 自身验证**

Run（jiade 根）:
```bash
go build ./... && go test ./...
```
Expected: 全绿（templates/bank 不参与 jiade build，但 generate 已重打包）。

- [ ] **Step 4: Commit**

```bash
git add internal/template/templates.tar
git commit -m "chore(bank): B-2 重打包 templates.tar"
```

---

## Self-Review（写后自查，已执行）

- **Spec coverage**：spec §5 三因子 → T1；§6 GenDay/DayState → T2；§7 RunBizDate/批量/切日 → T3；§8 替换快照 → T4；§9 GetBizDate/txn_service → T5；验收 #1-#8 → T1-T6 + 集成断言。gl_balance 跳过（非目标，无任务，正确）。确定性（§10）→ T1 volumeForDay 单测 + T2 GenDay 确定性 + 确定 txn_id。
- **Placeholder scan**：无 TBD/TODO；每步含完整代码或确切命令。
- **Type consistency**：`RunBizDate(ctx, db, cfg, demandNos)` 在 T3 定义、T4 调用一致；`GenDay(cfg, date, demandNos, st)` 跨 T2/T3 一致；`GetBizDate(ctx) (string, error)` 跨 T5 repo/iface/fakes 一致；fakes 返回 "2026-07-13" 与 T5 Step 8 断言一致。
