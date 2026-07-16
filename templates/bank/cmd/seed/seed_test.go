//go:build integration

package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
)

func TestMain(m *testing.M) {
	// runSeed 用相对模块根的路径读 db/migrations/*.sql；go test 把 CWD 设为包目录 cmd/seed/，
	// 切到模块根（templates/bank/）使集成测试可直接 `go test -tags=integration ./cmd/seed/` 运行。
	_, file, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Join(filepath.Dir(file), "..", "..")
	if err := os.Chdir(moduleRoot); err != nil {
		log.Fatalf("seed_test chdir %s: %v", moduleRoot, err)
	}
	os.Exit(m.Run())
}

func TestEnsureDBs_CreatesAllThree(t *testing.T) {
	ctx := context.Background()
	// 先确保 admin 可连
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres 管理库连接，跳过: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up）: %v", err)
	}
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db", "loan_db", "wealth_db"}); err != nil {
		t.Fatalf("ensureDBs 失败: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db", "reward_db", "risk_db", "loan_db", "wealth_db"} {
		var exists bool
		if err := admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("库 %s 未被创建", name)
		}
	}
}

func TestSeedRun_PopulatesAllDBs(t *testing.T) {
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres，跳过: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪: %v", err)
	}
	// 直接调 main 的编排函数（需把编排逻辑抽成 run()，见 Step 3）
	if err := runSeed(ctx, fixtures.DefaultConfig(fixtures.ScaleDev), true); err != nil {
		t.Fatalf("runSeed 失败: %v", err)
	}
	for _, c := range []struct{ db, table string }{
		{"cust_db", "cust_info"}, {"cust_db", "cust_account_rel"},
		{"pay_db", "merchant"}, {"pay_db", "transfer_txn"}, {"pay_db", "consumption_txn"},
		{"reward_db", "points_acct"}, {"reward_db", "points_txn"}, {"reward_db", "coupon"},
		{"risk_db", "risk_rule"}, {"risk_db", "risk_event"}, {"risk_db", "blacklist"},
		{"loan_db", "loan_product"}, {"loan_db", "loan_account"}, {"loan_db", "loan_repay"},
		{"loan_db", "loan_balance"}, {"loan_db", "loan_overdue"},
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
			t.Fatalf("查 %s.%s 失败（fdw/表未建？）: %v", c.db, c.table, err)
		}
		if n == 0 {
			t.Errorf("%s.%s 灌数据为空", c.db, c.table)
		}
	}
	// fdw 联邦表可查
	cust, _ := pg.Open("cust_db")
	defer cust.Close()
	if _, err := cust.ExecContext(ctx, "SELECT account_no FROM ext_core_db_demand_account LIMIT 1"); err != nil {
		t.Errorf("fdw 外部表不可查: %v", err)
	}
	// B-2: core 多日切日引擎
	coreDB2, err := pg.Open("core_db")
	if err != nil {
		t.Fatal(err)
	}
	defer coreDB2.Close()
	var bizDate string
	if err := coreDB2.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bizDate); err != nil {
		t.Fatalf("查 sys_param.biz_date: %v", err)
	}
	if bizDate != "2026-07-13" {
		t.Errorf("sys_param.biz_date=%q want 2026-07-13", bizDate)
	}
	var txnDays int
	if err := coreDB2.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&txnDays); err != nil {
		t.Fatalf("查 acct_txn 天数: %v", err)
	}
	if txnDays < 400 {
		t.Errorf("acct_txn 覆盖天数=%d, want ≥400", txnDays)
	}
	// 周末日均 < 工作日日均（cyclical ×0.60，聚合稳健）
	var wkAvg, wdAvg float64
	err = coreDB2.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM acct_txn GROUP BY biz_date) q`).Scan(&wkAvg, &wdAvg)
	if err != nil {
		t.Fatalf("查周末/工作日均值: %v", err)
	}
	if wkAvg >= wdAvg {
		t.Errorf("周末日均(%.0f) 应 < 工作日日均(%.0f)", wkAvg, wdAvg)
	}
	// B-4a: reward/risk 逐日三因子——周末日均 < 工作日日均
	rewardDB, err := pg.Open("reward_db")
	if err != nil {
		t.Fatal(err)
	}
	var rwWk, rwWd float64
	if err := rewardDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM points_txn GROUP BY biz_date) q`).Scan(&rwWk, &rwWd); err != nil {
		t.Fatalf("查 reward 周末/工作日均值: %v", err)
	}
	if rwWk == 0 || rwWk >= rwWd {
		t.Errorf("reward 周末日均(%.0f) 应 < 工作日(%.0f)", rwWk, rwWd)
	}
	rewardDB.Close()
	// reward/risk 联邦外部表可查
	riskDB, err := pg.Open("risk_db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := riskDB.ExecContext(ctx, "SELECT cust_id FROM ext_cust_db_cust_info LIMIT 1"); err != nil {
		t.Errorf("risk_db 联邦表 ext_cust_db_cust_info 不可查: %v", err)
	}
	riskDB.Close()
	// B-4b: loan 逐日滚存——loan_balance 末日有快照；逾期五级分类可滑且档位合法；联邦可 JOIN
	loanDB, err := pg.Open("loan_db")
	if err != nil {
		t.Fatal(err)
	}
	var eodBal int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_balance WHERE biz_date='2026-07-13'").Scan(&eodBal); err != nil {
		t.Fatalf("查 loan_balance 末日: %v", err)
	}
	if eodBal == 0 {
		t.Error("loan_balance 末日(2026-07-13)无快照行")
	}
	var classN, badClass int
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT overdue_class) FROM loan_overdue").Scan(&classN); err != nil {
		t.Fatalf("查 overdue_class: %v", err)
	}
	if classN < 2 {
		t.Errorf("逾期五级分类应随天数滑落至少 2 档, got %d", classN)
	}
	if err := loanDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM loan_overdue WHERE overdue_class NOT IN ('正常','关注','次级','可疑','损失')").Scan(&badClass); err != nil {
		t.Fatalf("查非法档位: %v", err)
	}
	if badClass > 0 {
		t.Errorf("loan_overdue 含非法五级分类 %d 行", badClass)
	}
	var loanCustName sql.NullString
	if err := loanDB.QueryRowContext(ctx, `SELECT ci.name FROM loan_account la
		JOIN ext_cust_db_cust_info ci ON la.cust_id=ci.cust_id LIMIT 1`).Scan(&loanCustName); err != nil {
		t.Errorf("loan_db 联邦 JOIN 不可查: %v", err)
	}
	loanDB.Close()
	// B-4b: wealth——nav 每产品每日有行；订单周末<工作日；income 覆盖全部持仓；联邦可 JOIN
	wealthDB, err := pg.Open("wealth_db")
	if err != nil {
		t.Fatal(err)
	}
	var navProds, navDays int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT product_code), COUNT(DISTINCT biz_date) FROM wealth_nav").Scan(&navProds, &navDays); err != nil {
		t.Fatalf("查 wealth_nav: %v", err)
	}
	if navProds != 6 || navDays < 400 {
		t.Errorf("wealth_nav 产品数=%d(应6) 天数=%d(应≥400)", navProds, navDays)
	}
	var wpWk, wpWd float64
	if err := wealthDB.QueryRowContext(ctx, `SELECT
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (0,6) THEN c END),
		AVG(CASE WHEN EXTRACT(DOW FROM biz_date) IN (1,2,3,4,5) THEN c END)
		FROM (SELECT biz_date, COUNT(*) c FROM wealth_order GROUP BY biz_date) q`).Scan(&wpWk, &wpWd); err != nil {
		t.Fatalf("查 wealth 周末/工作日均值: %v", err)
	}
	if wpWk == 0 || wpWk >= wpWd {
		t.Errorf("wealth 周末日均订单(%.0f) 应 < 工作日(%.0f)", wpWk, wpWd)
	}
	var incomeHoldings, incomeDays, holdingN int
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT holding_id), COUNT(DISTINCT biz_date) FROM wealth_income").Scan(&incomeHoldings, &incomeDays); err != nil {
		t.Fatalf("查 wealth_income: %v", err)
	}
	if err := wealthDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM wealth_holding").Scan(&holdingN); err != nil {
		t.Fatalf("查 wealth_holding: %v", err)
	}
	if incomeHoldings != holdingN || incomeDays < 400 {
		t.Errorf("wealth_income 覆盖持仓 %d/%d 天数 %d(应≥400)", incomeHoldings, holdingN, incomeDays)
	}
	var wealthCustName sql.NullString
	if err := wealthDB.QueryRowContext(ctx, `SELECT ci.name FROM wealth_holding h
		JOIN ext_cust_db_cust_info ci ON h.cust_id=ci.cust_id LIMIT 1`).Scan(&wealthCustName); err != nil {
		t.Errorf("wealth_db 联邦 JOIN 不可查: %v", err)
	}
	wealthDB.Close()
}
