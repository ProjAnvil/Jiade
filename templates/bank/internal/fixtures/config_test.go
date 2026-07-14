package fixtures

import "testing"

func TestTargetCounts(t *testing.T) {
	dev := DefaultConfig(ScaleDev).TargetCounts()
	if dev.DemandAccounts != 2000 {
		t.Errorf("dev demand=%d, want 2000", dev.DemandAccounts)
	}
	full := DefaultConfig(ScaleFull).TargetCounts()
	if full.DemandAccounts != 8000 {
		t.Errorf("full demand=%d, want 8000", full.DemandAccounts)
	}
	if full.DemandAccounts != 4*dev.DemandAccounts {
		t.Errorf("FULL 应为 DEV 的 4 倍")
	}
}
