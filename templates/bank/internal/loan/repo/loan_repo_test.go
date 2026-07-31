//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/loan/repo"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
)

type deterministicCustomerReader struct{}

var _ serviceclient.CustomerReader = deterministicCustomerReader{}

func (deterministicCustomerReader) GetCustomer(_ context.Context, customerID, _ string) (serviceclient.Customer, error) {
	return serviceclient.Customer{CustomerID: customerID, Name: "测试客户", CustomerType: "个人", KYCStatus: "passed", Status: "active"}, nil
}

func setupLoanDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("loan_db")
	if err != nil {
		t.Skipf("无 loan_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestLoanRepo_ListProducts(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	prods, err := repo.NewLoanRepo(db, deterministicCustomerReader{}).ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != 4 {
		t.Errorf("贷款产品应 4 个, got %d", len(prods))
	}
}

func TestLoanRepo_ListsAndDetail(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewLoanRepo(db, deterministicCustomerReader{})
	if _, err := r.ListAccounts(ctx, "", "", 0, 10); err != nil {
		t.Fatalf("ListAccounts 失败: %v", err)
	}
	if _, err := r.ListBalances(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListBalances 失败: %v", err)
	}
	if _, err := r.ListOverdue(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOverdue 失败: %v", err)
	}
	if _, err := r.GetAccount(ctx, "LN-NOPE"); err == nil {
		t.Error("不存在的借据应返回错误")
	}
}

func TestLoanRepo_GetProfile_NotFound(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	_, err := repo.NewLoanRepo(db, deterministicCustomerReader{}).GetProfile(context.Background(), "LN-NOPE")
	if err == nil {
		t.Error("不存在的借据应返回错误")
	}
}
