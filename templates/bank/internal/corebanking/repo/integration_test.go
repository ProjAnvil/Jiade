//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/repo"
	"bank/internal/corebanking/service"
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
	// Repeated applications should be cumulative
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
	// Create a historical daily balance row (baseline 500.00 yuan; column numeric(18,2), store value in "yuan")
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ('IT-D3','2026-07-15',500.00,500.00,'2011')`)

	// There are no rows on the current day (2026-07-16) → EnsureBalanceRow should inherit and return 500.00
	pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		b, err := lr.EnsureBalanceRow(ctx, q, "IT-D3", "2026-07-16", "2011")
		if err != nil {
			t.Fatal(err)
		}
		if b.Balance != domain.NewMoneyFromCents(50000) {
			t.Errorf("继承后余额=%s, want 500.00", b.Balance)
		}
		// Accumulated -100.00 → should be 400.00 on the day (not -100.00)
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

func TestRecord_Concurrent_NoDeadlock(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	for _, no := range []string{"CD-A", "CD-B"} {
		db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", no)
		ar.InsertDemand(ctx, domain.DemandAccount{
			AccountNo: no, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: "2026-07-15", SubjectCode: "2011",
		})
		// The column numeric(18,2) stores the value in "yuan"; 10000.00 yuan = 1000000 points (the assertion is in cents).
		db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
			VALUES ($1,'2026-07-15',10000.00,10000.00,'2011')`, no) // 10,000.00 yuan each
	}

	// The date of this test is assumed to be independent of the seed contract: Record writes the current day's snapshot according to sys_param.biz_date,
	// Must be later than the initial balance date 2026-07-15. Restore after testing to avoid contaminating other tests in the same library.
	var prevBizDate string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&prevBizDate); err != nil {
		t.Fatalf("读 biz_date: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE sys_param SET param_value='2026-07-16' WHERE param_key='biz_date'"); err != nil {
		t.Fatalf("调 biz_date: %v", err)
	}
	defer db.ExecContext(context.Background(), "UPDATE sys_param SET param_value=$1 WHERE param_key='biz_date'", prevBizDate)

	errs := make(chan error, 2)
	// T1: A→B; T2: B→A - without lock ordering, AB-BA deadlock will occur
	go func() {
		_, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-A", ToAccount: "CD-B", Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY"})
		errs <- e
	}()
	go func() {
		_, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-B", ToAccount: "CD-A", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY"})
		errs <- e
	}()

	for i := 0; i < 2; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("并发转账失败: %v", e)
		}
	}
	// Both transactions were successful: A balance 10000-100+50=9950.00; B balance 10000+100-50=10050.00
	tr := repo.NewTxnRepo(db)
	ba, _ := tr.GetLatestBalance(ctx, "CD-A")
	bb, _ := tr.GetLatestBalance(ctx, "CD-B")
	if ba.Balance != domain.NewMoneyFromCents(995000) {
		t.Errorf("A 余额=%s, want 9950.00", ba.Balance)
	}
	if bb.Balance != domain.NewMoneyFromCents(1005000) {
		t.Errorf("B 余额=%s, want 10050.00", bb.Balance)
	}
}

// TestReverse_Concurrent_DuplicateRejected Validation B-3 final review Important #1 Fixes:
// If two concurrent blueprints are issued for the same voucher, only one must be successful, and the other must return ErrAlreadyReversed;
// And the balance is only rolled back once (not twice - otherwise funds would be created out of thin air).
//
// Before fix: GetTxnsByVoucher None FOR UPDATE, UpdateTxnStatus None normal guard →
// Both concurrent blue rushes passed the "not reversed" check → double rollback → the balance was rolled back one more time = fund vulnerability.
func TestReverse_Concurrent_DuplicateRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	const acct = "RV-A"
	// clean history
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	// Create a historical day balance row (baseline 0.00 yuan) - EnsureBalanceRow must have a historical row to inherit to the current day.
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)

	// Deposit 100.00 (10,000 points) first to get a redeemable voucher
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// Read the balance before correction (on the day, it should be = 100.00 yuan)
	tr := repo.NewTxnRepo(db)
	balBefore, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("冲正前 %s 余额=%s（基线）", acct, balBefore.Balance)

	// Concurrently two blueprints for the same voucher
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = svc.Reverse(ctx, voucher, domain.ReverseBlue)
		}(i)
	}
	wg.Wait()

	// Must have exactly one success and one ErrAlreadyReversed
	ok, alreadyRev := 0, 0
	for i := 0; i < 2; i++ {
		if errs[i] == nil {
			ok++
		} else if errors.Is(errs[i], service.ErrAlreadyReversed) {
			alreadyRev++
		} else {
			t.Errorf("goroutine %d 返回非预期错误: %v", i, errs[i])
		}
	}
	if ok != 1 || alreadyRev != 1 {
		t.Fatalf("应恰好 1 成功 / 1 ErrAlreadyReversed, got ok=%d alreadyRev=%d errs=%v", ok, alreadyRev, errs)
	}

	// Critical Fund Security Assertion: The balance is only rolled back once (to 0), not twice (to -100.00 = -10000 points).
	balAfter, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// Original value 100.00 → Blue flush should be rolled back to 0 once. -100.00 if double rollback.
	if balAfter.Balance != domain.NewMoneyFromCents(0) {
		t.Errorf("蓝冲后余额=%s, want 0（回滚一次）；若为负值说明双回滚=资金漏洞", balAfter.Balance)
	}
	t.Logf("冲正后 %s 余额=%s（应为 0）", acct, balAfter.Balance)
}

// TestReverse_BlueThenRed_SecondRejected Verification: After the first blue flush is successful, the same voucher as the red flush should be rejected.
// Blue Chong changed txn_status to reversed. After red Chong entered, the line read by LockTxnsByVoucher was already reversed →
// Take the "any TxnStatus==reversed → ErrAlreadyReversed" branch.
func TestReverse_BlueThenRed_SecondRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	const acct = "RV-BR"
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// Blue rush first (should succeed)
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseBlue); err != nil {
		t.Fatalf("先蓝冲应成功: %v", err)
	}
	// Red rush again (the certificate has been reversed and should be rejected)
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("蓝冲后红冲同凭证应 ErrAlreadyReversed, got %v", err)
	}
}

// TestReverse_RedThenRed_SecondRejected Verification: After the first red flush is successful, the same voucher as the red flush should be rejected.
// Red flush does not change txn_status (spec §7.3), so rely on HasReversal(ref_txn_id) to remove duplicates:
// The reverse entry tape ref_txn_id of the first red post points to the original flow; the second post LockTxnsByVoucher is serialized
// HasReversal=true → ErrAlreadyReversed。
func TestReverse_RedThenRed_SecondRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	const acct = "RV-RR"
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// Red rush first (should succeed)
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); err != nil {
		t.Fatalf("先红冲应成功: %v", err)
	}
	// Redeem the same voucher again (HasReversal=true, should be rejected)
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("红冲后红冲同凭证应 ErrAlreadyReversed, got %v", err)
	}
}

// TestReverse_RedThenBlue_SecondRejected Verification B-3 fix2: After the first red flush is successful, the same voucher as the blue flush should be rejected.
// And the balance is only rolled back once (not twice).
//
// Before repair: red flush does not change txn_status (spec §7.3, the original flow is still normal), blue flush branch only relies on UpdateTxnStatus
// Normal guards are deduplicated - normal guards can still be matched after the first red rush → UpdateTxnStatus successful → reverseRollback
// One more rollback → Double rollback = money created out of thin air. The blue flush branch adds HasReversal(origs[0].TxnID) before UpdateTxnStatus.
// Check: Red flush has fallen ref_txn_id reverse entry → HasReversal=true → ErrAlreadyReversed, blue flush is rejected.
func TestReverse_RedThenBlue_SecondRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	const acct = "RV-RB"
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	// Baseline $0.00
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)
	// Deposit 100.00 (10,000 points) to get a redeemable voucher
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// Read the balance before red flush (should = 100.00 yuan)
	tr := repo.NewTxnRepo(db)
	balAfterRed, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("红冲前 %s 余额=%s（应为 100.00）", acct, balAfterRed.Balance)

	// Red flush first (should be successful: the original flow normal remains unchanged, the reverse entry is entered into the account, and the balance returns to 0)
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); err != nil {
		t.Fatalf("先红冲应成功: %v", err)
	}
	balAfterRed, err = tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// The red reverse entry offsets 100.00 → the balance returns to 0
	if balAfterRed.Balance != domain.NewMoneyFromCents(0) {
		t.Fatalf("红冲后余额=%s, want 0（红冲一次回滚后）", balAfterRed.Balance)
	}
	t.Logf("红冲后 %s 余额=%s（应为 0）", acct, balAfterRed.Balance)

	// Reprint the same voucher: HasReversal should be true → ErrAlreadyReversed
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseBlue); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("红冲后蓝冲同凭证应 ErrAlreadyReversed, got %v", err)
	}

	// Critical Fund Security Assertion: Balance is still 0 (rollback only once), not -100.00 (blue rush rolled back one more time = double rollback).
	balFinal, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if balFinal.Balance != domain.NewMoneyFromCents(0) {
		t.Errorf("红后蓝后余额=%s, want 0（蓝冲被拒，余额不变）；若为负值说明蓝冲双倍回滚=资金漏洞", balFinal.Balance)
	}
	t.Logf("红后蓝后 %s 余额=%s（应为 0，蓝冲被拒）", acct, balFinal.Balance)
}

// Compile-time assertion: ensure that *sql.DB unnecessary vars are referenced (avoid unused import).
var _ = pg.RunInTx
