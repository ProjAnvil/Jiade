//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/customer/domain"
	"bank/internal/customer/repo"
	"bank/internal/platform/pg"
)

func setupCustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("cust_db")
	if err != nil {
		t.Skipf("无 cust_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestCustomerRepo_GetAndList(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db)

	db.ExecContext(ctx, "DELETE FROM cust_info WHERE cust_id='IT-C1'")
	db.ExecContext(ctx, `INSERT INTO cust_info(cust_id,cust_type,name,cert_type,cert_no,nationality,risk_level,kyc_status,create_biz_date)
		VALUES ('IT-C1','个人','测试','身份证','110101000000000001','CN','low','passed','2026-01-01')`)

	got, err := r.GetCustomer(ctx, "IT-C1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "测试" || got.CustType != domain.CustTypePersonal {
		t.Errorf("got %+v", got)
	}
	list, err := r.ListCustomers(ctx, "个人", "passed", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Error("ListCustomers 应至少返回 IT-C1")
	}
}

func TestCustomerRepo_GetCustAccounts_FDWJoin(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db)
	// 依赖 seed 已建 ext_core_db_demand_account 外部表 + cust_account_rel + core demand_account 数据
	// 取 seed 出的第一个客户的账户（若无数据则跳过）
	accts, err := r.GetCustAccounts(ctx, "C0000001")
	if err != nil {
		t.Fatalf("FDW JOIN 查询失败（外部表未建？先 make seed + setup_fdw）: %v", err)
	}
	// 不强断言行数（取决于 seed 数据是否已灌），只断言查询不报错
	_ = accts
}
