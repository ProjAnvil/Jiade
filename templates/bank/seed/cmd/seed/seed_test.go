//go:build integration

package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"bank/seed/internal/fixtures"
	"bank/lib/pg"
)

func TestMain(m *testing.M) {
	// runSeed reads db/migrations/*.sql using a path relative to the workspace
	// root; go test sets CWD to the package directory seed/cmd/seed/. Cut to the
	// workspace root (templates/bank/, one level above the seed module) so that
	// the integration test can be run directly with
	// `go test -tags=integration ./cmd/seed/` from seed/.
	_, file, _, _ := runtime.Caller(0)
	workspaceRoot := filepath.Join(filepath.Dir(file), "..", "..", "..")
	if err := os.Chdir(workspaceRoot); err != nil {
		log.Fatalf("seed_test chdir %s: %v", workspaceRoot, err)
	}
	os.Exit(m.Run())
}

func TestEnsureDBs_CreatesAllDBs(t *testing.T) {
	ctx := context.Background()
	// First make sure admin can connect
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("no postgres admin connection, skipping: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make up first): %v", err)
	}
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db", "loan_db", "wealth_db"}); err != nil {
		t.Fatalf("ensureDBs failed: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db", "loan_db", "wealth_db"} {
		var exists bool
		if err := admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("database %s not created", name)
		}
	}
}

func TestSeedRun_PopulatesAllDBs(t *testing.T) {
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("no postgres, skipping: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres not ready: %v", err)
	}
	// Directly call the orchestration function of main (the orchestration logic needs to be extracted into run(), see Step 3)
	if err := runSeed(ctx, fixtures.DefaultConfig(fixtures.ScaleDev), true); err != nil {
		t.Fatalf("runSeed failed: %v", err)
	}
	for _, c := range []struct{ db, table string }{
		{"cust_db", "cust_info"}, {"cust_db", "cust_account_rel"},
		{"pay_db", "merchant"}, {"pay_db", "transfer_txn"}, {"pay_db", "consumption_txn"},
		{"reward_db", "points_acct"}, {"reward_db", "points_txn"}, {"reward_db", "coupon"},
		{"risk_db", "risk_rule"}, {"risk_db", "risk_event"}, {"risk_db", "blacklist"},
		{"loan_db", "loan_product"}, {"loan_db", "loan_account"}, {"loan_db", "loan_disbursement"},
		{"loan_db", "loan_repay"}, {"loan_db", "loan_balance"}, {"loan_db", "loan_overdue"},
		{"wealth_db", "wealth_product"}, {"wealth_db", "wealth_nav"}, {"wealth_db", "wealth_holding"},
		{"wealth_db", "wealth_order"}, {"wealth_db", "wealth_income"},
	} {
		db, err := pg.Open(c.db)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+c.table).Scan(&n)
		db.Close()
		if err != nil {
			t.Fatalf("query %s.%s failed (table not created?): %v", c.db, c.table, err)
		}
		if n == 0 {
			t.Errorf("%s.%s has no seeded data", c.db, c.table)
		}
	}
	// B-2: core multi-day cutting engine
	coreDB2, err := pg.Open("core_db")
	if err != nil {
		t.Fatal(err)
	}
	defer coreDB2.Close()
	var bizDate string
	if err := coreDB2.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bizDate); err != nil {
		t.Fatalf("query sys_param.biz_date: %v", err)
	}
	if bizDate != "2026-07-13" {
		t.Errorf("sys_param.biz_date=%q want 2026-07-13", bizDate)
	}
	var txnDays int
	if err := coreDB2.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&txnDays); err != nil {
		t.Fatalf("query acct_txn day count: %v", err)
	}
	if txnDays < 400 {
		t.Errorf("acct_txn days covered=%d, want ≥400", txnDays)
	}
	// Weekend daily average < weekday daily average (cyclical ×0.60, robust aggregation)
	var wkAvg, wdAvg float64
	err = coreDB2.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM acct_txn GROUP BY biz_date) q`).Scan(&wkAvg, &wdAvg)
	if err != nil {
		t.Fatalf("query weekend/weekday averages: %v", err)
	}
	if wkAvg >= wdAvg {
		t.Errorf("weekend daily avg(%.0f) should be < weekday daily avg(%.0f)", wkAvg, wdAvg)
	}
	// B-4a: reward/risk three factors on a daily basis - weekend daily average < weekday daily average
	rewardDB, err := pg.Open("reward_db")
	if err != nil {
		t.Fatal(err)
	}
	var rwWk, rwWd float64
	if err := rewardDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM points_txn GROUP BY biz_date) q`).Scan(&rwWk, &rwWd); err != nil {
		t.Fatalf("query reward weekend/weekday averages: %v", err)
	}
	if rwWk == 0 || rwWk >= rwWd {
		t.Errorf("reward weekend daily avg(%.0f) should be < weekday(%.0f)", rwWk, rwWd)
	}
	rewardDB.Close()
	// B-4b: Loans are rolled over day by day - loan_balance has a snapshot at the end of the day; the overdue five-level classification can be slid and the level is legal
	loanDB, err := pg.Open("loan_db")
	if err != nil {
		t.Fatal(err)
	}
	var eodBal int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_balance WHERE biz_date='2026-07-13'").Scan(&eodBal); err != nil {
		t.Fatalf("query loan_balance last day: %v", err)
	}
	if eodBal == 0 {
		t.Error("loan_balance last day(2026-07-13) has no snapshot rows")
	}
	var classN, badClass int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT overdue_class) FROM loan_overdue").Scan(&classN); err != nil {
		t.Fatalf("query overdue_class: %v", err)
	}
	if classN < 2 {
		t.Errorf("overdue five-tier classification should drop at least 2 tiers with days, got %d", classN)
	}
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_overdue WHERE overdue_class NOT IN ('正常','关注','次级','可疑','损失')").Scan(&badClass); err != nil {
		t.Fatalf("query invalid tier: %v", err)
	}
	if badClass > 0 {
		t.Errorf("loan_overdue has %d rows with invalid five-tier classification", badClass)
	}
	loanDB.Close()
	// B-4b: wealth——nav Each product has daily rows; orders are on weekends < working days; income covers all positions
	wealthDB, err := pg.Open("wealth_db")
	if err != nil {
		t.Fatal(err)
	}
	var navProds, navDays int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT product_code), COUNT(DISTINCT biz_date) FROM wealth_nav").Scan(&navProds, &navDays); err != nil {
		t.Fatalf("query wealth_nav: %v", err)
	}
	if navProds != 6 || navDays < 400 {
		t.Errorf("wealth_nav products=%d(want 6) days=%d(want ≥400)", navProds, navDays)
	}
	var wpWk, wpWd float64
	if err := wealthDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM wealth_order GROUP BY biz_date) q`).Scan(&wpWk, &wpWd); err != nil {
		t.Fatalf("query wealth weekend/weekday averages: %v", err)
	}
	if wpWk == 0 || wpWk >= wpWd {
		t.Errorf("wealth weekend daily orders(%.0f) should be < weekday(%.0f)", wpWk, wpWd)
	}
	var incomeHoldings, incomeDays, holdingN int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT holding_id), COUNT(DISTINCT biz_date) FROM wealth_income").Scan(&incomeHoldings, &incomeDays); err != nil {
		t.Fatalf("query wealth_income: %v", err)
	}
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM wealth_holding").Scan(&holdingN); err != nil {
		t.Fatalf("query wealth_holding: %v", err)
	}
	if incomeHoldings != holdingN || incomeDays < 400 {
		t.Errorf("wealth_income covers holdings %d/%d days %d(want ≥400)", incomeHoldings, holdingN, incomeDays)
	}
	wealthDB.Close()
}
