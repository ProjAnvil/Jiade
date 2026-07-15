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
}
