//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/repo"
	"bank/internal/platform/pg"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("core_db")
	if err != nil {
		t.Skipf("无 core_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up）: %v", err)
	}
	return db
}

func TestAccountRepo_InsertAndGetDemand(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)

	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='IT-D1'")
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: "IT-D1", CustID: "C1", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ar.GetDemand(ctx, "IT-D1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CustID != "C1" || got.Status != domain.AccountStatusActive {
		t.Errorf("got cust_id=%s status=%s", got.CustID, got.Status)
	}
}

func TestLedgerRepo_BalanceDelta_Accumulates(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)

	for _, no := range []string{"IT-D1", "IT-D2"} {
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", no)
		ar.InsertDemand(ctx, domain.DemandAccount{
			AccountNo: no, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: "2026-07-15", SubjectCode: "2011",
		})
	}
	deltas := []domain.BalanceDelta{
		{AccountNo: "IT-D1", Delta: domain.NewMoneyFromCents(10000), SubjectCode: "2011"},
		{AccountNo: "IT-D2", Delta: domain.NewMoneyFromCents(-10000), SubjectCode: "2011"},
	}
	if err := lr.ApplyBalanceDeltas(ctx, "2026-07-15", deltas); err != nil {
		t.Fatal(err)
	}
	// 重复应用应累加
	if err := lr.ApplyBalanceDeltas(ctx, "2026-07-15", deltas); err != nil {
		t.Fatal(err)
	}
	tr := repo.NewTxnRepo(db)
	b, err := tr.GetLatestBalance(ctx, "IT-D1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Balance != domain.NewMoneyFromCents(20000) {
		t.Errorf("累加后余额=%s, want 200.00", b.Balance)
	}
}
