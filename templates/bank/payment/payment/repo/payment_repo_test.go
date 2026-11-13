//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/payment/payment/repo"
	"bank/lib/pg"
	"bank/lib/serviceclient"
)

type deterministicAccountReader struct{}

var _ serviceclient.AccountReader = deterministicAccountReader{}

func (deterministicAccountReader) GetAccount(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
	return serviceclient.Account{AccountNo: accountNo, CustomerID: "C-test", Currency: "CNY", Status: "active"}, nil
}

type deterministicCustomerReader struct{}

var _ serviceclient.CustomerReader = deterministicCustomerReader{}

func (deterministicCustomerReader) GetCustomer(_ context.Context, customerID, _ string) (serviceclient.Customer, error) {
	return serviceclient.Customer{CustomerID: customerID, Name: "test customer", CustomerType: "personal", KYCStatus: "passed", Status: "active"}, nil
}

func setupPayDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("pay_db")
	if err != nil {
		t.Skipf("no pay_db connection, skipping: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping (run make seed first): %v", err)
	}
	return db
}

func TestPaymentRepo_GetMerchant(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db, deterministicAccountReader{}, deterministicCustomerReader{})
	db.ExecContext(ctx, "DELETE FROM merchant WHERE merchant_id='IT-M1'")
	db.ExecContext(ctx, `INSERT INTO merchant(merchant_id,merchant_name,mcc,region,status,create_biz_date)
		VALUES ('IT-M1','test merchant','5411','East China','active','2026-01-01')`)
	m, err := r.GetMerchant(ctx, "IT-M1")
	if err != nil {
		t.Fatal(err)
	}
	if m.MerchantName != "test merchant" {
		t.Errorf("got %+v", m)
	}
}

func TestPaymentRepo_TransfersAndParties(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db, deterministicAccountReader{}, deterministicCustomerReader{})
	_, err := r.ListTransfers(ctx, "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListTransfers failed: %v", err)
	}
	// Cross-service aggregation uses deterministic readers; only transfer storage is database-backed.
	_, err = r.GetTransferParties(ctx, "PT000000000001")
	if err != nil {
		t.Fatalf("GetTransferParties cross-service aggregation failed (run make up first): %v", err)
	}
}

func TestPaymentRepo_GetTransfer_NotFound(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	_, err := repo.NewPaymentRepo(db, deterministicAccountReader{}, deterministicCustomerReader{}).GetTransfer(context.Background(), "NOPE")
	if err == nil {
		t.Error("should return error (not found)")
	}
}
