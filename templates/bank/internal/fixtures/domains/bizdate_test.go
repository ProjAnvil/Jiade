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
	// 年末 12-25 ×1.50（季末需 day≥28，故 25 号仅年末 spike）
	if got := seasonalFactor(parseDate("2025-12-25")); got != 1.50 {
		t.Errorf("年末 12-25 应 1.50, got %v", got)
	}
	// 12-28 季末×1.35 × 年末×1.50（叠加；用运行时乘法避免常量折叠精度差异）
	qEnd, yearEnd := 1.35, 1.50
	wantDec28 := qEnd * yearEnd
	if got := seasonalFactor(parseDate("2025-12-28")); got != wantDec28 {
		t.Errorf("12-28 季末+年末应 %v, got %v", wantDec28, got)
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
