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
	// 20% Public: j%5==0 → 0th public
	if a[0].CustType != "对公" {
		t.Errorf("j=0 应对公，got %s", a[0].CustType)
	}
	if a[1].CustType != "个人" {
		t.Errorf("j=1 应个人，got %s", a[1].CustType)
	}
}

func TestGenCustomersAssignsDeterministicPreparationRiskTags(t *testing.T) {
	customers := GenCustomers(fixtures.DefaultConfig(fixtures.ScaleDev), 100)
	var highRisk int
	for _, customer := range customers {
		if customer.Status != "active" {
			t.Fatalf("customer %s status = %q, want active", customer.CustID, customer.Status)
		}
		if customer.RiskLevel == "high" {
			highRisk++
			if len(customer.RiskTags) != 1 || customer.RiskTags[0] != "high-risk" {
				t.Fatalf("high-risk customer tags = %#v, want [high-risk]", customer.RiskTags)
			}
		}
		if customer.RiskLevel == "low" && customer.RiskTags == nil {
			t.Fatalf("low-risk customer %s must persist an empty (not nil) tag array", customer.CustID)
		}
	}
	if highRisk == 0 {
		t.Fatal("deterministic fixture must contain high-risk preparation customers")
	}
}

func TestGenAccountRels_LinksCustToAccount(t *testing.T) {
	pairs := [][2]string{{"C0000001", "D0000000001"}, {"C0000001", "D0000000002"}}
	rels := GenAccountRels(pairs)
	if len(rels) != 2 || rels[0].CustID != "C0000001" || rels[0].AccountNo != "D0000000001" {
		t.Errorf("rel 关联错误: %+v", rels)
	}
}
