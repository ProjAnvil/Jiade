//go:build integration

package fdw_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/fdw"
	"bank/internal/platform/pg"
)

func mustAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up + make seed 建 3 库表）: %v", err)
	}
	return db
}

func TestSetupFDW_CreatesForeignTables(t *testing.T) {
	ctx := context.Background()
	admin := mustAdmin(t)
	defer admin.Close()
	// 前置：3 库表已存在（Task 2 的 ensureDBs + migrate 应已跑过；此处防御性建表）
	if err := fdw.SetupFDW(ctx); err != nil {
		t.Fatalf("SetupFDW 失败: %v", err)
	}
	// 在 cust_db 应能查到 ext_core_db_demand_account 外部表（来自 core_db.demand_account）
	cust, err := pg.Open("cust_db")
	if err != nil {
		t.Fatal(err)
	}
	defer cust.Close()
	if _, err := cust.ExecContext(ctx, "SELECT account_no FROM ext_core_db_demand_account LIMIT 1"); err != nil {
		t.Errorf("cust_db 查 ext_core_db_demand_account 失败，FDW 未建好: %v", err)
	}
	// 幂等：再跑一次不报错
	if err := fdw.SetupFDW(ctx); err != nil {
		t.Errorf("SetupFDW 二次运行应幂等，失败: %v", err)
	}
}
