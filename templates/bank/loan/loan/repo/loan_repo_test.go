//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/loan/loan/repo"
	"bank/lib/pg"
	"bank/lib/serviceclient"
)

type deterministicCustomerReader struct{}

var _ serviceclient.CustomerReader = deterministicCustomerReader{}

func (deterministicCustomerReader) GetCustomer(_ context.Context, customerID, _ string) (serviceclient.Customer, error) {
	return serviceclient.Customer{CustomerID: customerID, Name: "test customer", CustomerType: "personal", KYCStatus: "passed", Status: "active"}, nil
}

func setupLoanDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("loan_db")
	if err != nil {
		t.Skipf("no loan_db connection, skipping: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make seed first): %v", err)
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
		t.Errorf("loan products should be 4, got %d", len(prods))
	}
}

func TestLoanRepo_ListsAndDetail(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewLoanRepo(db, deterministicCustomerReader{})
	if _, err := r.ListAccounts(ctx, "", "", 0, 10); err != nil {
		t.Fatalf("ListAccounts failed: %v", err)
	}
	if _, err := r.ListBalances(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListBalances failed: %v", err)
	}
	if _, err := r.ListOverdue(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOverdue failed: %v", err)
	}
	if _, err := r.GetAccount(ctx, "LN-NOPE"); err == nil {
		t.Error("nonexistent loan instrument should return error")
	}
}

func TestLoanRepo_GetProfile_NotFound(t *testing.T) {
	db := setupLoanDB(t)
	defer db.Close()
	_, err := repo.NewLoanRepo(db, deterministicCustomerReader{}).GetProfile(context.Background(), "LN-NOPE")
	if err == nil {
		t.Error("nonexistent loan instrument should return error")
	}
}
