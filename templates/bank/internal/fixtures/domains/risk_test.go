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
