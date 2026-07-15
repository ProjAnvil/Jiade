package domain

import "testing"

func TestCustomerTypes(t *testing.T) {
	if CustTypePersonal != "个人" || CustTypeOrg != "对公" {
		t.Errorf("客户类型常量错误: %q %q", CustTypePersonal, CustTypeOrg)
	}
	c := Customer{CustID: "C0000001", CustType: CustTypePersonal, Name: "张伟", KYCStatus: "passed"}
	if c.CustID != "C0000001" {
		t.Errorf("cust_id=%s", c.CustID)
	}
}

func TestParseCustAccount(t *testing.T) {
	a := CustAccount{AccountNo: "D0000000001", Ccy: "CNY", Status: "active", Role: "主"}
	if a.AccountNo != "D0000000001" {
		t.Errorf("account_no=%s", a.AccountNo)
	}
}
