//go:build integration

package domains

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"bank/seed/internal/fixtures"
	"bank/lib/migrate"
	"bank/lib/pg"
)

// setupCoreDB rebuilds core_db and creates tables (destructive: DROP core_db). If there is no pg, skip.
func setupCoreDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("no postgres: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres not ready (run make up first): %v", err)
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
	// seed/internal/fixtures/domains → bank root is 4 levels up.
	ddl, err := os.ReadFile("../../../../db/migrations/core_db.sql")
	if err != nil {
		t.Skipf("read core_db.sql failed (cwd?): %v", err)
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
		t.Fatalf("query biz_date: %v", err)
	}
	if bd != "2025-06-30" {
		t.Errorf("sys_param.biz_date=%q want 2025-06-30", bd)
	}
	var days int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days); err != nil {
		t.Fatal(err)
	}
	if days != 30 {
		t.Errorf("acct_txn days covered=%d want 30", days)
	}
	var bal int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_balance").Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if bal != 30*20 {
		t.Errorf("account_balance rows=%d want %d", bal, 30*20)
	}
	// Idempotent: Run twice, the number of days remains unchanged (DELETE+INSERT daily)
	if err := RunBizDate(ctx, db, cfg, nos); err != nil {
		t.Fatalf("second RunBizDate: %v", err)
	}
	var days2 int
	db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT biz_date) FROM acct_txn").Scan(&days2)
	if days2 != 30 {
		t.Errorf("days after second run=%d want 30 (should be idempotent)", days2)
	}
}
