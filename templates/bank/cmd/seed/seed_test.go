//go:build integration

package main

import (
	"context"
	"testing"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
)

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
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db"}); err != nil {
		t.Fatalf("ensureDBs 失败: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db"} {
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
}
