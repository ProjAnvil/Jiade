package domain

import "testing"

func TestRiskEvent(t *testing.T) {
	e := RiskEvent{EventID: "E1", CustID: "C1", RiskScore: "0.73", ActionTaken: "拦截"}
	if e.CustID != "C1" || e.RiskScore != "0.73" {
		t.Errorf("got %+v", e)
	}
}

func TestRiskRule(t *testing.T) {
	r := RiskRule{RuleID: "R001", Action: "拦截", Threshold: "100000.00"}
	if r.Action != "拦截" {
		t.Errorf("got %+v", r)
	}
}
