package domains

// Unit tests for the per-day orchestrators (RunBizDate, RunLoan, RunReward,
// RunRisk, RunWealth) using the fakesql harness: a short 2-day config drives
// the delete/generate/bulk-insert/commit loop for every domain without a live
// PostgreSQL. Guard branches (empty inputs, bad date ranges) are asserted
// directly.

import (
	"context"
	"strings"
	"testing"

	"bank/seed/internal/fixtures"

	loanDomain "bank/loan/loan/domain"
	rewardDomain "bank/reward/reward/domain"
	wealthDomain "bank/wealth/wealth/domain"
)

func shortConfig() fixtures.Config {
	return fixtures.Config{
		StartBizDate: "2026-07-01",
		EndBizDate:   "2026-07-02",
		Scale:        fixtures.ScaleDev,
		Seed:         7,
	}
}

func orchestratorDB() *fakeDB {
	db := &fakeDB{}
	db.add(queryPlan{marker: "delete from", affected: 0})
	db.add(queryPlan{marker: "insert into", affected: 1})
	return db
}

func TestRunBizDate_TwoDaysAndSysParamSwitch(t *testing.T) {
	db := orchestratorDB()
	if err := RunBizDate(context.Background(), db.open(), shortConfig(), []string{"D1", "D2", "D3"}); err != nil {
		t.Fatalf("RunBizDate: %v", err)
	}
	// Two day-txs: each day deletes its txn + balance rows then bulk-inserts.
	if got := len(db.callsFor("delete from acct_txn")); got != 2 {
		t.Errorf("per-day txn clears = %d, want 2", got)
	}
	if got := len(db.callsFor("delete from account_balance")); got != 2 {
		t.Errorf("per-day balance clears = %d, want 2", got)
	}
	// The final statement flips sys_param to the last day.
	call := db.lastCallWith("insert into sys_param")
	if call == nil || call.args[0] != "2026-07-02" || call.args[1] != "2026-07-01" {
		t.Errorf("biz_date switch wrong: %+v", call)
	}
	// One tx per day must commit.
	_, commits, _ := db.counters()
	if commits < 2 {
		t.Errorf("commits = %d, want >= 2", commits)
	}
}

func TestRunBizDate_Guards(t *testing.T) {
	db := orchestratorDB()
	if err := RunBizDate(context.Background(), db.open(), shortConfig(), nil); err == nil {
		t.Error("empty demand accounts must fail")
	}
	bad := shortConfig()
	bad.EndBizDate = "2025-01-01"
	if err := RunBizDate(context.Background(), db.open(), bad, []string{"D1"}); err == nil {
		t.Error("reversed date range must fail")
	}
}

func TestRunReward_GuardsAndLoop(t *testing.T) {
	db := orchestratorDB()
	if err := RunReward(context.Background(), db.open(), shortConfig(), nil, nil); err == nil || !strings.Contains(err.Error(), "no points account") {
		t.Fatalf("empty points accounts must fail, got %v", err)
	}
	accts := []rewardDomain.PointsAcct{{CustID: "C1", PointsBalance: 1000}, {CustID: "C2", PointsBalance: 500}}
	if err := RunReward(context.Background(), db.open(), shortConfig(), accts, []string{"CAM-1"}); err != nil {
		t.Fatalf("RunReward: %v", err)
	}
	if got := len(db.callsFor("delete from points_txn")); got != 2 {
		t.Errorf("per-day txn clears = %d, want 2", got)
	}
	if got := len(db.callsFor("insert into points_txn")); got != 2 {
		t.Errorf("per-day txn batches = %d, want 2", got)
	}
}

func TestRunRisk_Loop(t *testing.T) {
	db := orchestratorDB()
	if err := RunRisk(context.Background(), db.open(), shortConfig(), []string{"C1", "C2"}, []string{"A1", "A2"}); err != nil {
		t.Fatalf("RunRisk: %v", err)
	}
	if got := len(db.callsFor("delete from risk_event")); got != 2 {
		t.Errorf("per-day clears = %d, want 2", got)
	}
	if got := len(db.callsFor("insert into risk_event")); got != 2 {
		t.Errorf("per-day batches = %d, want 2", got)
	}
}

func TestRunLoan_Loop(t *testing.T) {
	db := orchestratorDB()
	accounts := []loanDomain.LoanAccount{
		{LoanNo: "LN1", CustID: "C1", ProductCode: "LP-CONS", Ccy: "CNY", Principal: 1200000, Balance: 900000, StartBizDate: "2026-01-10", MatureDate: "2029-01-10", TermMonths: 36},
		{LoanNo: "LN2", CustID: "C2", ProductCode: "LP-CONS", Ccy: "CNY", Principal: 600000, Balance: 300000, StartBizDate: "2026-02-10", MatureDate: "2027-02-10", TermMonths: 12},
	}
	if err := RunLoan(context.Background(), db.open(), shortConfig(), accounts); err != nil {
		t.Fatalf("RunLoan: %v", err)
	}
	// Repay plans are generated only at month rollover: a mid-month 2-day
	// window produces exactly one plan batch (day 1 of the loop).
	if got := len(db.callsFor("delete from loan_repay")); got != 1 {
		t.Errorf("repay clears = %d, want 1 (month-start only)", got)
	}
	if got := len(db.callsFor("insert into loan_repay")); got != 1 {
		t.Errorf("repay batches = %d, want 1 (month-start only)", got)
	}
	if got := len(db.callsFor("delete from loan_balance")); got != 2 {
		t.Errorf("per-day balance clears = %d, want 2", got)
	}
}

func TestRunWealth_Loop(t *testing.T) {
	db := orchestratorDB()
	products := []wealthDomain.WealthProduct{{ProductCode: "WP-FIX1", ProductName: "Fixed", ProductType: "fixed", RiskLevel: "low", ExpectedReturn: "0.035000", MinAmount: 100000, TermDays: 365, StartBizDate: "2026-01-01", EndBizDate: "2027-01-01", Status: "active"}}
	holdings := []wealthDomain.WealthHolding{{HoldingID: "H1", CustID: "C1", AccountNo: "D1", ProductCode: "WP-FIX1", Ccy: "CNY", Share: "100.0000", Cost: 100000, CurrentValue: 101000, BizDate: "2026-01-05"}}
	if err := RunWealth(context.Background(), db.open(), shortConfig(), products, holdings, []string{"C1"}, []string{"D1"}); err != nil {
		t.Fatalf("RunWealth: %v", err)
	}
	if got := len(db.callsFor("delete from wealth_nav")); got != 2 {
		t.Errorf("per-day nav clears = %d, want 2", got)
	}
	if got := len(db.callsFor("insert into wealth_nav")); got != 2 {
		t.Errorf("per-day nav batches = %d, want 2", got)
	}
}
