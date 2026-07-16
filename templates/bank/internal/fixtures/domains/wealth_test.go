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
