package domains

import (
	"reflect"
	"testing"

	"bank/seed/internal/fixtures"
)

func TestGenLoanStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003", "C0000004", "C0000005", "C0000006", "C0000007", "C0000008"}
	a := GenLoanStatic(cfg, custIDs)
	b := GenLoanStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenLoanStatic not deterministic")
	}
	if len(a.Products) != 4 {
		t.Errorf("products should be 4, got %d", len(a.Products))
	}
	if len(a.Accounts) != maxInt(5, len(custIDs)/4) { // 8/4=2 → maxInt(5,2)=5
		t.Errorf("loan instruments=%d, want 5", len(a.Accounts))
	}
	if len(a.Disbursements) != len(a.Accounts) {
		t.Error("disbursement count should equal instrument count")
	}
	acct := a.Accounts[0]
	if acct.LoanNo != "LN0000000" {
		t.Errorf("loan_no=%s want LN0000000", acct.LoanNo)
	}
	if acct.Principal.Cents() < 10000*100 {
		t.Errorf("principal should be ≥10000 yuan, got %d cents", acct.Principal.Cents())
	}
	if acct.Balance != acct.Principal {
		t.Error("initial balance should equal principal")
	}
	if acct.MatureDate != addMonths(acct.StartBizDate, acct.TermMonths) {
		t.Errorf("mature=%s should be start+term", acct.MatureDate)
	}
	if a.Disbursements[0].DisbID != "LN-DB-0000000" || a.Disbursements[0].Amount != acct.Principal {
		t.Errorf("disbursement wrong: %+v", a.Disbursements[0])
	}
	if len(acct.Rate) != 8 { // "0.043500" 6dp
		t.Errorf("rate should be 6dp text, got %q", acct.Rate)
	}
}

func TestOverdueClass(t *testing.T) {
	cases := []struct {
		days int
		want string
	}{
		{0, "正常"}, {1, "关注"}, {29, "关注"}, {30, "次级"}, {89, "次级"},
		{90, "可疑"}, {179, "可疑"}, {180, "损失"}, {365, "损失"},
	}
	for _, c := range cases {
		if got := overdueClass(c.days); got != c.want {
			t.Errorf("overdueClass(%d)=%s want %s", c.days, got, c.want)
		}
	}
}

func TestRoundDiv(t *testing.T) {
	if roundDiv(100, 12) != 8 { // 8.33→8
		t.Errorf("roundDiv(100,12)=%d want 8", roundDiv(100, 12))
	}
	if roundDiv(101, 2) != 51 { // 50.5→51
		t.Errorf("roundDiv(101,2)=%d want 51", roundDiv(101, 2))
	}
}

func TestMaxDateStr(t *testing.T) {
	if maxDateStr("2025-06-01", "2026-06-13") != "2026-06-13" {
		t.Error("maxDateStr should take the larger")
	}
	if maxDateStr("2026-07-13", "2025-01-01") != "2026-07-13" {
		t.Error("maxDateStr short-range guard failed")
	}
}
