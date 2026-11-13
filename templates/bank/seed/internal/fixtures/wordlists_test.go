package fixtures

import "testing"

func TestB4bWordlists(t *testing.T) {
	if len(LoanProducts) != 4 {
		t.Errorf("LoanProducts should have 4, got %d", len(LoanProducts))
	}
	if LoanProducts[0].Code != "LP-CONS" || LoanProducts[1].MaxAmountYuan != 5000000 {
		t.Errorf("LoanProducts tuple wrong: %+v", LoanProducts[0])
	}
	if len(WealthProducts) != 6 {
		t.Errorf("WealthProducts should have 6, got %d", len(WealthProducts))
	}
	if WealthProducts[0].Code != "WP-FIX1" || WealthProducts[5].MinAmountYuan != 1000 {
		t.Errorf("WealthProducts tuple wrong: %+v", WealthProducts[5])
	}
	if len(OverdueClasses) != 5 {
		t.Fatalf("OverdueClasses should have 5 tiers, got %d", len(OverdueClasses))
	}
	for i := 1; i < len(OverdueClasses); i++ {
		if OverdueClasses[i].Days <= OverdueClasses[i-1].Days {
			t.Errorf("OverdueClasses days should be ascending: %+v", OverdueClasses)
		}
	}
	if OverdueClasses[4].Name != "损失" {
		t.Errorf("tier 5 should be the loss tier, got %s", OverdueClasses[4].Name)
	}
	if len(GuaranteeTypes) != 3 || len(OrderTypes) != 3 || len(IncomeTypes) != 1 {
		t.Error("GuaranteeTypes/OrderTypes/IncomeTypes wrong length")
	}
	if OrderTypes[0] != "申购" || OrderTypes[2] != "赎回" {
		t.Errorf("OrderTypes wrong: %v", OrderTypes)
	}
}
