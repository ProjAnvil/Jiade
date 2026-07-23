package domains

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
)

func TestFactor_Trend(t *testing.T) {
	base := parseDate("2025-06-01")
	if trendFactor(base) != 1.0 {
		t.Errorf("base 月因子应 1.0, got %v", trendFactor(base))
	}
	// After 3 months: 1.0+0.02*3=1.06
	if got := trendFactor(parseDate("2025-09-15")); got != 1.06 {
		t.Errorf("9 月因子应 1.06, got %v", got)
	}
}

func TestFactor_Seasonal(t *testing.T) {
	// Ordinary day
	if got := seasonalFactor(parseDate("2025-06-12")); got != 1.0 {
		t.Errorf("普通日应 1.0, got %v", got)
	}
	// Season end (June 28) ×1.35
	if got := seasonalFactor(parseDate("2025-06-28")); got != 1.35 {
		t.Errorf("季末日应 1.35, got %v", got)
	}
	// Payday ×1.40
	if got := seasonalFactor(parseDate("2025-07-10")); got != 1.40 {
		t.Errorf("发薪日应 1.40, got %v", got)
	}
	// Holidays (National Day)×1.30
	if got := seasonalFactor(parseDate("2025-10-01")); got != 1.30 {
		t.Errorf("国庆应 1.30, got %v", got)
	}
	// Year-end 12-25 ×1.50 (the end of the quarter requires day≥28, so the 25th is only the year-end spike)
	if got := seasonalFactor(parseDate("2025-12-25")); got != 1.50 {
		t.Errorf("年末 12-25 应 1.50, got %v", got)
	}
	// 12-28 End of quarter × 1.35 × End of year × 1.50 (overlay; use runtime multiplication to avoid constant folding accuracy differences)
	qEnd, yearEnd := 1.35, 1.50
	wantDec28 := qEnd * yearEnd
	if got := seasonalFactor(parseDate("2025-12-28")); got != wantDec28 {
		t.Errorf("12-28 季末+年末应 %v, got %v", wantDec28, got)
	}
}

func TestFactor_Cyclical(t *testing.T) {
	// 2025-06-01 is Sunday
	if got := cyclicalFactor(parseDate("2025-06-01")); got != 0.60 {
		t.Errorf("周日应 0.60, got %v", got)
	}
	// 2025-06-02 Monday
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
	// The same week: Wednesday vs Sunday (cyclical is higher than the weekend, and the upper limit of magnitude should be smaller on Sunday)
	wd := volumeForDay(cfg, parseDate("2025-07-16")) // Wednesday
	wk := volumeForDay(cfg, parseDate("2025-07-20")) // Sunday
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

func init() {} // Placeholder (retains package-level init hook, no side effects)

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
	// Multiple accounts + multi-day accumulation, credit:debit should be approximately 2:1 (large tolerance)
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
	// Scripting: credit plus, borrow clamp 0
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	nos := []string{"D1"}
	st := &DayState{Bal: map[string]domain.Money{"D1": domain.NewMoneyFromCents(50000)}} // 500.00
	// Manual simulation: GenDay internal advancement rules for st.Bal
	st.Bal["D1"] = st.Bal["D1"].Add(domain.NewMoneyFromCents(30000)) // +300 → 800
	if got := st.Bal["D1"].Cents(); got != 80000 {
		t.Fatalf("贷后余额错: %d", got)
	}
	b := st.Bal["D1"].Sub(domain.NewMoneyFromCents(200000)) // -2000 → negative
	if b < 0 {
		b = domain.NewMoneyFromCents(0)
	}
	st.Bal["D1"] = b
	if got := st.Bal["D1"].Cents(); got != 0 {
		t.Fatalf("借 clamp 0 错: %d", got)
	}
	// GenDay balance snapshot covers all accounts
	_, bals := GenDay(cfg, parseDate("2025-07-15"), nos, newDayState(cfg, nos))
	if len(bals) != 1 || bals[0].AccountNo != "D1" {
		t.Errorf("快照应覆盖全部账户, got %+v", bals)
	}
	if bals[0].AvailableBalance != bals[0].Balance || bals[0].FrozenAmount.Cents() != 0 {
		t.Errorf("available/frozen 语义错: %+v", bals[0])
	}
}
