//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/lib/pg"
	"bank/lib/serviceclient"
	"bank/wealth/wealth/repo"
)

type deterministicCustomerReader struct{}

var _ serviceclient.CustomerReader = deterministicCustomerReader{}

func (deterministicCustomerReader) GetCustomer(_ context.Context, customerID, _ string) (serviceclient.Customer, error) {
	return serviceclient.Customer{CustomerID: customerID, Name: "test customer", CustomerType: "personal", KYCStatus: "passed", Status: "active"}, nil
}

func setupWealthDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("wealth_db")
	if err != nil {
		t.Skipf("no wealth_db connection, skipping: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make seed first): %v", err)
	}
	return db
}

func TestWealthRepo_ListProducts(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	prods, err := repo.NewWealthRepo(db, deterministicCustomerReader{}).ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != 6 {
		t.Errorf("wealth products should be 6, got %d", len(prods))
	}
}

func TestWealthRepo_Lists(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewWealthRepo(db, deterministicCustomerReader{})
	if _, err := r.ListNav(ctx, "", "", ""); err != nil {
		t.Fatalf("ListNav failed: %v", err)
	}
	if _, err := r.ListHoldings(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListHoldings failed: %v", err)
	}
	if _, err := r.ListOrders(ctx, "", "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOrders failed: %v", err)
	}
	if _, err := r.ListIncomes(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListIncomes failed: %v", err)
	}
}

func TestWealthRepo_GetHoldingProfile_NotFound(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	_, err := repo.NewWealthRepo(db, deterministicCustomerReader{}).GetHoldingProfile(context.Background(), "WP-HD-NOPE")
	if err == nil {
		t.Error("nonexistent holding should return error")
	}
}
