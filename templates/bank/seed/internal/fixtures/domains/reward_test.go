package domains

import (
	"reflect"
	"testing"

	"bank/seed/internal/fixtures"
)

func TestGenRewardStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003"}
	a := GenRewardStatic(cfg, custIDs)
	b := GenRewardStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenRewardStatic not deterministic")
	}
	if len(a.PointsAccts) != 3 || a.PointsAccts[0].CustID != "C0000001" {
		t.Errorf("points_acct wrong: %+v", a.PointsAccts)
	}
	if len(a.MemberLevels) != 5 {
		t.Errorf("member_level should have 5 tiers, got %d", len(a.MemberLevels))
	}
}

func TestGenRewardStatic_ScaleCampaigns(t *testing.T) {
	dev := GenRewardStatic(fixtures.DefaultConfig(fixtures.ScaleDev), []string{"C1"})
	full := GenRewardStatic(fixtures.DefaultConfig(fixtures.ScaleFull), []string{"C1"})
	if len(full.Campaigns) <= len(dev.Campaigns) {
		t.Errorf("full campaigns(%d) should be > dev(%d)", len(full.Campaigns), len(dev.Campaigns))
	}
}
