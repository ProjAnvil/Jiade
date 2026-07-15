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
	if err := lr.ApplyBalanceDeltas(ctx, db, "2026-07-15", deltas); err != nil {
		t.Fatal(err)
	}
	// 重复应用应累加
	if err := lr.ApplyBalanceDeltas(ctx, db, "2026-07-15", deltas); err != nil {
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

func TestLedgerRepo_EnsureBalanceRow_InheritsAcrossDate(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)

	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='IT-D3'")
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no='IT-D3'")
	ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: "IT-D3", CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	})
	// 建一个历史日余额行（基线 500.00）
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ('IT-D3','2026-07-15',50000,50000,'2011')`)

	// 当天（2026-07-16）无行 → EnsureBalanceRow 应继承并返回 500.00
	pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		b, err := lr.EnsureBalanceRow(ctx, q, "IT-D3", "2026-07-16", "2011")
		if err != nil {
			t.Fatal(err)
		}
		if b.Balance != domain.NewMoneyFromCents(50000) {
			t.Errorf("继承后余额=%s, want 500.00", b.Balance)
		}
		// 累加 -100.00 → 当天应 400.00（非 -100.00）
		lr.ApplyBalanceDeltas(ctx, q, "2026-07-16", []domain.BalanceDelta{
			{AccountNo: "IT-D3", Delta: domain.NewMoneyFromCents(-10000), SubjectCode: "2011"},
		})
		return nil
	})
	tr := repo.NewTxnRepo(db)
	b, err := tr.GetLatestBalance(ctx, "IT-D3")
	if err != nil {
		t.Fatal(err)
	}
	if b.Balance != domain.NewMoneyFromCents(40000) {
		t.Errorf("继承+累加后余额=%s, want 400.00", b.Balance)
	}
}
