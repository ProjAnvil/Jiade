package domains

// Unit tests for the bulk-insert helpers across every fixture domain using
// the fakesql harness. Each helper must skip empty slices, batch rows, marshal
// Money as NUMERIC strings, NULL out empty strings, and wrap failures.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"bank/core-banking/corebanking/domain"
	loanDomain "bank/loan/loan/domain"
	rewardDomain "bank/reward/reward/domain"
	riskDomain "bank/risk/risk/domain"
	wealthDomain "bank/wealth/wealth/domain"
)

var bulkNow = time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)

// NOTE: several fixture domains share a type name (Money, Txn...). Import
// aliases keep the row literals readable below.

func bulkDB() *fakeDB {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into", affected: 1})
	return db
}

func failingBulkDB() *fakeDB {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into", err: errors.New("batch failed")})
	return db
}

func TestBulkInsertTxns_MarshalsAndWraps(t *testing.T) {
	db := bulkDB()
	rows := []domain.Txn{
		{TxnID: "T1", BizDate: "2026-07-31", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.Money(10000), Ccy: "CNY", SubjectCode: "1001"},
		{TxnID: "T2", BizDate: "2026-07-31", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.Money(10000), Ccy: "CNY", SubjectCode: "1001"},
	}
	if err := bulkInsertTxns(context.Background(), db.open(), rows); err != nil {
		t.Fatalf("bulkInsertTxns: %v", err)
	}
	call := db.lastCallWith("insert into acct_txn")
	if call == nil || len(call.args) != 22 {
		t.Fatalf("args = %d, want 22 (2 rows x 11 cols)", len(call.args))
	}
	if call.args[4] != "100.00" {
		t.Errorf("amount must serialise as NUMERIC string, got %#v", call.args[4])
	}
	if call.args[7] != nil {
		t.Errorf("empty opp_account must NULL, got %#v", call.args[7])
	}

	if err := bulkInsertTxns(context.Background(), db.open(), nil); err != nil {
		t.Errorf("empty slice must be a no-op: %v", err)
	}
	if err := bulkInsertTxns(context.Background(), failingBulkDB().open(), rows); err == nil || !strings.Contains(err.Error(), "batch failed") {
		t.Errorf("want wrapped error, got %v", err)
	}
}

func TestBulkInsertBalances(t *testing.T) {
	db := bulkDB()
	rows := []domain.Balance{
		{AccountNo: "D1", BizDate: "2026-07-31", Balance: 100000, AvailableBalance: 90000, FrozenAmount: 10000, SubjectCode: "1001"},
	}
	if err := bulkInsertBalances(context.Background(), db.open(), rows); err != nil {
		t.Fatalf("bulkInsertBalances: %v", err)
	}
	if got := len(db.callsFor("insert into account_balance")); got != 1 {
		t.Errorf("balance batches = %d, want 1", got)
	}
}

func TestBulkInsertLoanTables(t *testing.T) {
	db := bulkDB()
	ctx := context.Background()
	repays := []loanDomain.LoanRepay{{RepayID: "R1", BizDate: "2026-07-31", LoanNo: "LN1", PrincipalAmt: 83333, InterestAmt: 3625}}
	balances := []loanDomain.LoanBalance{{LoanNo: "LN1", BizDate: "2026-07-31", PrincipalBalance: 900000, InterestReceivable: 3625}}
	overdues := []loanDomain.LoanOverdue{{OverdueID: "OD1", BizDate: "2026-07-31", LoanNo: "LN1", OverdueDays: 35, OverdueClass: "substandard", OverdueAmount: 123456}}

	if err := bulkInsertLoanRepays(ctx, db.open(), repays); err != nil {
		t.Fatalf("bulkInsertLoanRepays: %v", err)
	}
	if err := bulkInsertLoanBalances(ctx, db.open(), balances); err != nil {
		t.Fatalf("bulkInsertLoanBalances: %v", err)
	}
	if err := bulkInsertLoanOverdues(ctx, db.open(), overdues); err != nil {
		t.Fatalf("bulkInsertLoanOverdues: %v", err)
	}
	if got := len(db.callsFor("insert into loan_repay")); got != 1 {
		t.Errorf("repay batches = %d", got)
	}
	if got := len(db.callsFor("insert into loan_balance")); got != 1 {
		t.Errorf("balance batches = %d", got)
	}
	if got := len(db.callsFor("insert into loan_overdue")); got != 1 {
		t.Errorf("overdue batches = %d", got)
	}
}

func TestBulkInsertRewardTables(t *testing.T) {
	db := bulkDB()
	ctx := context.Background()
	txns := []rewardDomain.PointsTxn{{TxnID: "PT1", CustID: "C1", BizDate: "2026-07-31", Points: 100, Direction: "earn", SourceType: "payment"}}
	coupons := []rewardDomain.Coupon{{CouponID: "CP1", CustID: "C1", FaceValue: 5000, MinSpend: 30000, Status: "issued", IssueBizDate: "2026-07-31", ExpireDate: "2026-12-31"}}

	if err := bulkInsertPointsTxns(ctx, db.open(), txns); err != nil {
		t.Fatalf("bulkInsertPointsTxns: %v", err)
	}
	if err := bulkInsertCoupons(ctx, db.open(), coupons); err != nil {
		t.Fatalf("bulkInsertCoupons: %v", err)
	}
	if got := len(db.callsFor("insert into points_txn")); got != 1 {
		t.Errorf("points batches = %d", got)
	}
	if got := len(db.callsFor("insert into coupon")); got != 1 {
		t.Errorf("coupon batches = %d", got)
	}
}

func TestBulkInsertRiskEvents(t *testing.T) {
	db := bulkDB()
	rows := []riskDomain.RiskEvent{{EventID: "E1", BizDate: "2026-07-31", CustID: "C1", RuleID: "R-KYC", RiskScore: "0.850000", ActionTaken: "reject", Summary: "kyc"}}
	if err := bulkInsertRiskEvents(context.Background(), db.open(), rows); err != nil {
		t.Fatalf("bulkInsertRiskEvents: %v", err)
	}
	if got := len(db.callsFor("insert into risk_event")); got != 1 {
		t.Errorf("risk batches = %d", got)
	}
}

func TestBulkInsertWealthTables(t *testing.T) {
	db := bulkDB()
	ctx := context.Background()
	navs := []wealthDomain.WealthNav{{ProductCode: "WP-FIX1", BizDate: "2026-07-31", Nav: "1.023400", AccumNav: "1.254300"}}
	orders := []wealthDomain.WealthOrder{{OrderID: "WO1", BizDate: "2026-07-31", CustID: "C1", ProductCode: "WP-FIX1", OrderType: "subscribe", Amount: 5000000, Share: "50.0000", Nav: "1.023400", Status: "confirmed"}}
	incomes := []wealthDomain.WealthIncome{{IncomeID: "WI1", BizDate: "2026-07-31", HoldingID: "H1", IncomeType: "daily", Amount: 1234}}

	if err := bulkInsertWealthNavs(ctx, db.open(), navs); err != nil {
		t.Fatalf("bulkInsertWealthNavs: %v", err)
	}
	if err := bulkInsertWealthOrders(ctx, db.open(), orders); err != nil {
		t.Fatalf("bulkInsertWealthOrders: %v", err)
	}
	if err := bulkInsertWealthIncomes(ctx, db.open(), incomes); err != nil {
		t.Fatalf("bulkInsertWealthIncomes: %v", err)
	}
	if got := len(db.callsFor("insert into wealth_nav")); got != 1 {
		t.Errorf("nav batches = %d", got)
	}
	if got := len(db.callsFor("insert into wealth_order")); got != 1 {
		t.Errorf("order batches = %d", got)
	}
	if got := len(db.callsFor("insert into wealth_income")); got != 1 {
		t.Errorf("income batches = %d", got)
	}
}
