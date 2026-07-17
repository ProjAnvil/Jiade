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
