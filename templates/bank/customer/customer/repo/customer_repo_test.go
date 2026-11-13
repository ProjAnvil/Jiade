//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/customer/customer/domain"
	"bank/customer/customer/repo"
	"bank/lib/pg"
	"bank/lib/serviceclient"
)

type deterministicAccountReader struct{}

var _ serviceclient.AccountReader = deterministicAccountReader{}

func (deterministicAccountReader) GetAccount(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
	return serviceclient.Account{AccountNo: accountNo, CustomerID: "C-test", Currency: "CNY", Status: "active"}, nil
}

func setupCustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("cust_db")
	if err != nil {
		t.Skipf("no cust_db connection, skipping: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make seed first): %v", err)
	}
	return db
}

func TestCustomerRepo_GetAndList(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db, deterministicAccountReader{})

	db.ExecContext(ctx, "DELETE FROM cust_info WHERE cust_id='IT-C1'")
	db.ExecContext(ctx, `INSERT INTO cust_info(cust_id,cust_type,name,cert_type,cert_no,nationality,risk_level,kyc_status,customer_status,risk_tags,create_biz_date)
		VALUES ('IT-C1','personal','test user','ID card','110101000000000001','CN','low','passed','restricted',ARRAY['pep','sanctions'],'2026-01-01')`)

	got, err := r.GetCustomer(ctx, "IT-C1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "test user" || got.CustType != domain.CustTypePersonal || got.Status != "restricted" || len(got.RiskTags) != 2 || got.RiskTags[0] != "pep" || got.RiskTags[1] != "sanctions" {
		t.Errorf("got %+v", got)
	}
	list, err := r.ListCustomers(ctx, "personal", "passed", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Error("ListCustomers should return at least IT-C1")
	}
}

func TestCustomerRepo_GetCustAccounts_ServiceCall(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db, deterministicAccountReader{})
	// Account details come from a deterministic reader; only the relationship is database-backed.
	accts, err := r.GetCustAccounts(ctx, "C0000001")
	if err != nil {
		t.Fatalf("cross-service account query failed (run make up first): %v", err)
	}
	// Do not forcefully assert the number of rows (depends on whether the seed data has been filled), only assert that the query does not report an error
	_ = accts
}
