//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/lib/pg"
	"bank/lib/serviceclient"
	"bank/risk/risk/repo"
)

type deterministicCustomerReader struct{}

var _ serviceclient.CustomerReader = deterministicCustomerReader{}

func (deterministicCustomerReader) GetCustomer(_ context.Context, customerID, _ string) (serviceclient.Customer, error) {
	return serviceclient.Customer{CustomerID: customerID, Name: "test customer", CustomerType: "personal", KYCStatus: "passed", Status: "active"}, nil
}

func setupRiskDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("risk_db")
	if err != nil {
		t.Skipf("no risk_db connection, skipping: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make seed first): %v", err)
	}
	return db
}

func TestRiskRepo_ListRules(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	rules, err := repo.NewRiskRepo(db, deterministicCustomerReader{}).ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Error("ListRules should return rules (5 after seed)")
	}
}

func TestRiskRepo_ListEventsAndBlacklists(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewRiskRepo(db, deterministicCustomerReader{})
	if _, err := r.ListEvents(ctx, "", "", "", "", 0, 10); err != nil {
		t.Fatalf("ListEvents failed: %v", err)
	}
	if _, err := r.ListBlacklists(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListBlacklists failed: %v", err)
	}
}

func TestRiskRepo_GetEvent_NotFound(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	_, err := repo.NewRiskRepo(db, deterministicCustomerReader{}).GetEvent(context.Background(), "RS-EV-NOPE")
	if err == nil {
		t.Error("nonexistent event should return error")
	}
}
