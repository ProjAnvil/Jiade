package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenLoanStatic_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	custIDs := []string{"C0000001", "C0000002", "C0000003", "C0000004", "C0000005", "C0000006", "C0000007", "C0000008"}
	a := GenLoanStatic(cfg, custIDs)
	b := GenLoanStatic(cfg, custIDs)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenLoanStatic 不确定")
	}
	if len(a.Products) != 4 {
		t.Errorf("产品应 4 个, got %d", len(a.Products))
	}
	if len(a.Accounts) != maxInt(5, len(custIDs)/4) { // 8/4=2 → maxInt(5,2)=5
		t.Errorf("借据数=%d, want 5", len(a.Accounts))
	}
	if len(a.Disbursements) != len(a.Accounts) {
		t.Errorf("放款数应=借据数")
	}
	acct := a.Accounts[0]
	if acct.LoanNo != "LN0000000" {
		t.Errorf("loan_no=%s want LN0000000", acct.LoanNo)
	}
	if acct.Principal.Cents() < 10000*100 {
		t.Errorf("本金应 ≥10000 元, got %d 分", acct.Principal.Cents())
	}
	if acct.Balance != acct.Principal {
		t.Error("初始余额应=本金")
	}
	if acct.MatureDate != addMonths(acct.StartBizDate, acct.TermMonths) {
		t.Errorf("mature=%s 应=start+term", acct.MatureDate)
	}
	if a.Disbursements[0].DisbID != "LN-DB-0000000" || a.Disbursements[0].Amount != acct.Principal {
		t.Errorf("放款错: %+v", a.Disbursements[0])
	}
	if len(acct.Rate) != 8 { // "0.043500" 6dp
		t.Errorf("rate 应 6dp 文本, got %q", acct.Rate)
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
		t.Error("maxDateStr 应取大者")
	}
	if maxDateStr("2026-07-13", "2025-01-01") != "2026-07-13" {
		t.Error("maxDateStr 短区间守卫")
	}
}
