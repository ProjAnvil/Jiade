package domain

import "testing"

func TestLoanAccount(t *testing.T) {
	a := LoanAccount{LoanNo: "LN0000000", CustID: "C0000001", Principal: NewMoneyFromCents(1000000), Balance: NewMoneyFromCents(1000000), Rate: "0.043500"}
	if a.LoanNo != "LN0000000" || a.Principal.String() != "10000.00" {
		t.Errorf("got %+v", a)
	}
}

func TestLoanRepayMoneyRoundTrip(t *testing.T) {
	r := LoanRepay{RepayID: "LN-RP-20250601-00000", PrincipalAmt: NewMoneyFromCents(83333), InterestAmt: NewMoneyFromCents(3625)}
	if r.PrincipalAmt.String() != "833.33" || r.InterestAmt.String() != "36.25" {
		t.Errorf("repay 金额错: %s %s", r.PrincipalAmt, r.InterestAmt)
	}
	p, err := ParseCents("833.33")
	if err != nil || p != r.PrincipalAmt {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}
