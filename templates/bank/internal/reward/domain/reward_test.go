package domain

import "testing"

func TestPointsAcct(t *testing.T) {
	a := PointsAcct{CustID: "C0000001", PointsBalance: 500, MemberLevel: "L2"}
	if a.CustID != "C0000001" || a.PointsBalance != 500 {
		t.Errorf("got %+v", a)
	}
}

func TestCouponMoney(t *testing.T) {
	c := Coupon{CouponID: "CP1", FaceValue: NewMoneyFromCents(2000), MinSpend: NewMoneyFromCents(5000)}
	if c.FaceValue.String() != "20.00" || c.MinSpend.String() != "50.00" {
		t.Errorf("coupon 金额错: %s %s", c.FaceValue, c.MinSpend)
	}
}
