//go:build integration

package domains

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"bank/internal/fixtures"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
)

// setupCoreDB 重建 core_db 并建表（破坏性：DROP core_db）。无 pg 则 skip。
func setupCoreDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪（先 make up）: %v", err)
	}
	admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='core_db' AND pid<>pg_backend_pid()")
	if _, err := admin.ExecContext(ctx, `DROP DATABASE IF EXISTS "core_db"`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "core_db"`); err != nil {
		t.Fatal(err)
	}
	db, err := pg.Open("core_db")
	if err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile("../../../db/migrations/core_db.sql") // domains → bank 根 3 级上溯
	if err != nil {
		t.Skipf("读 core_db.sql 失败（cwd?）: %v", err)
	}
	if err := migrate.Run(ctx, db, string(ddl)); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRunBizDate_WritesAllDaysAndCutsSysParam(t *testing.T) {
	ctx := context.Background()
	db := setupCoreDB(t)
	defer db.Close()
	cfg := fixtures.Config{StartBizDate: "2025-06-01", EndBizDate: "2025-06-30", Scale: fixtures.ScaleDev, Seed: 42}
	nos := make([]string, 20)
	for i := range nos {
		nos[i] = fmt.Sprintf("D%010d", i+1)
	}
	if err := RunBizDate(ctx, db, cfg, nos); err != nil {
		t.Fatalf("RunBizDate: %v", err)
	}
	var bd string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bd); err != nil {
		t.Fatalf("查 biz_date: %v", err)
	}
	if bd != "2025-06-30" {
		t.Errorf("sys_param.biz_date=%q want 2025-06-30", bd)
	}
	var days int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days != 30 {
		t.Errorf("acct_txn 覆盖天数=%d want 30", days)
	}
	var bal int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_balance").Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != 30*20 {
		t.Errorf("account_balance 行=%d want %d", bal, 30*20)
	}
	// 幂等：二次跑，天数不变（逐日 DELETE+INSERT）
	if err := RunBizDate(ctx, db, cfg, nos); err != nil {
		t.Fatalf("二次 RunBizDate: %v", err)
	}
	var days2 int
	db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days2)
	if days2 != 30 {
		t.Errorf("二次跑后天数=%d want 30（应幂等）", days2)
	}
}
