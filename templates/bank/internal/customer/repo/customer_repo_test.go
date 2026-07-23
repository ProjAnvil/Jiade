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

func TestCustomerRepo_GetCustAccounts_ServiceCall(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db)
	// Depends on seed data and started core-banking service.
	accts, err := r.GetCustAccounts(ctx, "C0000001")
	if err != nil {
		t.Fatalf("跨服务账户查询失败（先 make up）: %v", err)
	}
	// Do not forcefully assert the number of rows (depends on whether the seed data has been filled), only assert that the query does not report an error
	_ = accts
}
