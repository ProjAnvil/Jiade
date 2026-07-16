package domain

import "testing"

func TestWealthHolding(t *testing.T) {
	h := WealthHolding{HoldingID: "WP-HD-0000000", CustID: "C0000001", Share: "1050.2500", Cost: NewMoneyFromCents(100000)}
	if h.HoldingID != "WP-HD-0000000" || h.Cost.String() != "1000.00" {
		t.Errorf("got %+v", h)
	}
}

func TestWealthOrderMoneyRoundTrip(t *testing.T) {
	o := WealthOrder{OrderID: "WP-OD-20250601-00000", Amount: NewMoneyFromCents(500050), Nav: "1.023456"}
	if o.Amount.String() != "5000.50" {
		t.Errorf("order 金额错: %s", o.Amount)
	}
	p, err := ParseCents("5000.50")
	if err != nil || p != o.Amount {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}
