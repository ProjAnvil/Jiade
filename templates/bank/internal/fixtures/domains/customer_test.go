package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenCustomers_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	a := GenCustomers(cfg, 20)
	b := GenCustomers(cfg, 20)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenCustomers 不确定性")
	}
	if len(a) != 20 || a[0].CustID != "C0000001" {
		t.Errorf("首行 cust_id=%s len=%d", a[0].CustID, len(a))
	}
	// 20% 对公：j%5==0 → 第 0 个对公
	if a[0].CustType != "对公" {
		t.Errorf("j=0 应对公，got %s", a[0].CustType)
	}
	if a[1].CustType != "个人" {
		t.Errorf("j=1 应个人，got %s", a[1].CustType)
	}
}

func TestGenAccountRels_LinksCustToAccount(t *testing.T) {
	pairs := [][2]string{{"C0000001", "D0000000001"}, {"C0000001", "D0000000002"}}
	rels := GenAccountRels(pairs)
	if len(rels) != 2 || rels[0].CustID != "C0000001" || rels[0].AccountNo != "D0000000001" {
		t.Errorf("rel 关联错误: %+v", rels)
	}
}
