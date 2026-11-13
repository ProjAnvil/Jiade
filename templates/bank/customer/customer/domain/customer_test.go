package domain

import "testing"

func TestCustomerTypes(t *testing.T) {
	if CustTypePersonal != "personal" || CustTypeOrg != "corporate" {
		t.Errorf("customer type constants wrong: %q %q", CustTypePersonal, CustTypeOrg)
	}
	c := Customer{CustID: "C0000001", CustType: CustTypePersonal, Name: "Zhang Wei", KYCStatus: "passed"}
	if c.CustID != "C0000001" {
		t.Errorf("cust_id=%s", c.CustID)
	}
}

func TestParseCustAccount(t *testing.T) {
	a := CustAccount{AccountNo: "D0000000001", Ccy: "CNY", Status: "active", Role: "primary"}
	if a.AccountNo != "D0000000001" {
		t.Errorf("account_no=%s", a.AccountNo)
	}
}
