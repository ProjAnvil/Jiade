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
	"bank/internal/platform/messaging"
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

// ---------------------------------------------------------------------------
// Held transfer + red reversal integration tests (Task 3)
// ---------------------------------------------------------------------------

// NOTE: The held_transfer, voucher_reversal, and outbox_message tables must
// exist in the test core_db (created by the migration or manually granted).
// The bank DB user typically lacks CREATE TABLE privileges, so tests assume
// the schema is pre-provisioned — matching the convention of all existing
// corebanking integration tests.

// outboxFailingLedgerRepo wraps *repo.LedgerRepo and injects an AppendOutbox
// failure to verify that the entire PostHeldTransfer transaction rolls back.
type outboxFailingLedgerRepo struct {
	*repo.LedgerRepo
	failOutbox bool
}

func (r *outboxFailingLedgerRepo) AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error {
	if r.failOutbox {
		return errors.New("injected outbox failure")
	}
	return r.LedgerRepo.AppendOutbox(ctx, q, env, routingKey)
}

// setupHeldTransferAccounts seeds two active accounts (D1 with 200.00 balance,
// D2 with 0.00 balance), advances biz_date to 2026-07-16, and returns a
// cleanup. Both accounts are cleaned of prior txn/balance/hold rows.
func setupHeldTransferAccounts(t *testing.T, db *sql.DB, fromAcct, toAcct string) {
	t.Helper()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)

	for _, no := range []string{fromAcct, toAcct} {
		db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", no)
		if err := ar.InsertDemand(ctx, domain.DemandAccount{
			AccountNo: no, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: "2026-07-15", SubjectCode: "2011",
		}); err != nil {
			t.Fatalf("seed account %s: %v", no, err)
		}
	}
	// Seed D1 historical balance 200.00 yuan; D2 starts at 0.00.
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',200.00,200.00,'2011')`, fromAcct)
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, toAcct)

	// Advance biz_date so EnsureBalanceRow inherits the historical rows.
	var prev string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&prev); err != nil {
		t.Fatalf("读 biz_date: %v", err)
	}
	db.ExecContext(ctx, "UPDATE sys_param SET param_value='2026-07-16' WHERE param_key='biz_date'")
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "UPDATE sys_param SET param_value=$1 WHERE param_key='biz_date'", prev)
	})
}

// TestHeldTransfer_Post_CapturesHold_PostsBalancedEntries verifies the full
// PostHeldTransfer orchestration against PG: the hold is captured, two
// balanced entries are written, balances are moved, and the outbox row is
// committed in the same transaction.
func TestHeldTransfer_Post_CapturesHold_PostsBalancedEntries(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const from, to = "HT-A", "HT-B"
	setupHeldTransferAccounts(t, db, from, to)
	db.ExecContext(ctx, "DELETE FROM funds_hold WHERE account_no=$1", from)
	db.ExecContext(ctx, "DELETE FROM held_transfer WHERE idempotency_key='K1'")
	db.ExecContext(ctx, "DELETE FROM voucher_reversal WHERE reverses_voucher_no LIKE 'V%'")
	db.ExecContext(ctx, "DELETE FROM outbox_message WHERE message_type LIKE 'core.%'")

	hr := repo.NewHoldRepo(db)
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)
	ls := service.NewLedgerService(lr)
	holdSvc := service.NewHoldService(db, hr)
	transferSvc := service.NewHeldTransferService(db, hr, ar, ls, lr, lr)

	// Place a 50.00-yuan hold (5000 cents) on the from-account.
	hold, err := holdSvc.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "PH1", AccountNo: from,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY", WorkflowID: "WF1",
	})
	if err != nil {
		t.Fatalf("PlaceHold: %v", err)
	}

	// Post the held transfer.
	booking, err := transferSvc.PostHeldTransfer(ctx, service.PostHeldTransfer{
		IdempotencyKey: "K1", HoldID: hold.HoldID,
		FromAccount: from, ToAccount: to,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PostHeldTransfer: %v", err)
	}
	if booking.VoucherNo == "" {
		t.Fatal("应返回非空 voucher_no")
	}

	// Exactly two entries under the voucher.
	var entryCount int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM acct_txn WHERE voucher_no=$1", booking.VoucherNo).Scan(&entryCount); err != nil {
		t.Fatal(err)
	}
	if entryCount != 2 {
		t.Errorf("应 2 条流水, got %d", entryCount)
	}

	// Hold captured in DB.
	var holdStatus string
	if err := db.QueryRowContext(ctx,
		"SELECT status FROM funds_hold WHERE hold_id=$1", hold.HoldID).Scan(&holdStatus); err != nil {
		t.Fatal(err)
	}
	if holdStatus != string(domain.HoldStatusCaptured) {
		t.Errorf("hold status=%q want captured", holdStatus)
	}

	// Balance moved: from=200-50=150.00, to=0+50=50.00.
	tr := repo.NewTxnRepo(db)
	bf, _ := tr.GetLatestBalance(ctx, from)
	bt, _ := tr.GetLatestBalance(ctx, to)
	if bf.Balance != domain.NewMoneyFromCents(15000) {
		t.Errorf("from 余额=%s, want 150.00", bf.Balance)
	}
	if bt.Balance != domain.NewMoneyFromCents(5000) {
		t.Errorf("to 余额=%s, want 50.00", bt.Balance)
	}

	// Outbox row committed.
	var outboxCount int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM outbox_message WHERE message_type=$1", service.EventTransferPosted).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Errorf("outbox count=%d, want 1", outboxCount)
	}
}

// TestHeldTransfer_DuplicateIdempotencyKey_ReturnsSameVoucher verifies that
// re-posting with the same idempotency key returns the previously committed
// voucher without creating a second set of entries.
func TestHeldTransfer_DuplicateIdempotencyKey_ReturnsSameVoucher(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const from, to = "HT-D1", "HT-D2"
	setupHeldTransferAccounts(t, db, from, to)
	db.ExecContext(ctx, "DELETE FROM funds_hold WHERE account_no=$1", from)
	db.ExecContext(ctx, "DELETE FROM held_transfer WHERE idempotency_key='DK1'")
	db.ExecContext(ctx, "DELETE FROM outbox_message WHERE message_type LIKE 'core.%'")

	hr := repo.NewHoldRepo(db)
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)
	ls := service.NewLedgerService(lr)
	holdSvc := service.NewHoldService(db, hr)
	transferSvc := service.NewHeldTransferService(db, hr, ar, ls, lr, lr)

	hold, err := holdSvc.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "PH-D1", AccountNo: from,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PlaceHold: %v", err)
	}

	in := service.PostHeldTransfer{
		IdempotencyKey: "DK1", HoldID: hold.HoldID,
		FromAccount: from, ToAccount: to,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	}
	b1, err := transferSvc.PostHeldTransfer(ctx, in)
	if err != nil {
		t.Fatalf("first PostHeldTransfer: %v", err)
	}
	b2, err := transferSvc.PostHeldTransfer(ctx, in)
	if err != nil {
		t.Fatalf("duplicate PostHeldTransfer: %v", err)
	}
	if b1.VoucherNo != b2.VoucherNo {
		t.Fatalf("应返回同一 voucher, got %q vs %q", b1.VoucherNo, b2.VoucherNo)
	}

	// Exactly 2 entries (not 4).
	var entryCount int
	db.QueryRowContext(ctx, "SELECT count(*) FROM acct_txn WHERE voucher_no=$1", b1.VoucherNo).Scan(&entryCount)
	if entryCount != 2 {
		t.Errorf("重复幂等键不应多写流水, got %d entries", entryCount)
	}
}

// TestHeldTransfer_OutboxFailure_RollsBack verifies the critical ACID
// guarantee: when the outbox insert fails, the entire transaction rolls back
// — no entries, no balance changes, and the hold stays active.
func TestHeldTransfer_OutboxFailure_RollsBack(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const from, to = "HT-RB1", "HT-RB2"
	setupHeldTransferAccounts(t, db, from, to)
	db.ExecContext(ctx, "DELETE FROM funds_hold WHERE account_no=$1", from)
	db.ExecContext(ctx, "DELETE FROM held_transfer WHERE idempotency_key='RBK1'")
	db.ExecContext(ctx, "DELETE FROM outbox_message WHERE message_type LIKE 'core.%'")

	hr := repo.NewHoldRepo(db)
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)
	ls := service.NewLedgerService(lr)
	holdSvc := service.NewHoldService(db, hr)

	// Wrap the ledger repo so AppendOutbox fails.
	failingLR := &outboxFailingLedgerRepo{LedgerRepo: lr, failOutbox: true}
	transferSvc := service.NewHeldTransferService(db, hr, ar, ls, failingLR, failingLR)

	hold, err := holdSvc.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "PH-RB1", AccountNo: from,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PlaceHold: %v", err)
	}

	_, err = transferSvc.PostHeldTransfer(ctx, service.PostHeldTransfer{
		IdempotencyKey: "RBK1", HoldID: hold.HoldID,
		FromAccount: from, ToAccount: to,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err == nil {
		t.Fatal("Outbox 失败应返回 error")
	}

	// Critical rollback assertions: no entries written.
	var entryCount int
	db.QueryRowContext(ctx, "SELECT count(*) FROM acct_txn WHERE account_no IN ($1,$2)", from, to).Scan(&entryCount)

	// NOTE: $1,$2 doesn't work with IN in pgx; use two counts.
	var c1, c2 int
	db.QueryRowContext(ctx, "SELECT count(*) FROM acct_txn WHERE account_no=$1", from).Scan(&c1)
	db.QueryRowContext(ctx, "SELECT count(*) FROM acct_txn WHERE account_no=$1", to).Scan(&c2)
	if c1+c2 != 0 {
		t.Errorf("回滚后不应有流水, from=%d to=%d", c1, c2)
	}

	// Hold still active (not captured).
	var holdStatus string
	db.QueryRowContext(ctx, "SELECT status FROM funds_hold WHERE hold_id=$1", hold.HoldID).Scan(&holdStatus)
	if holdStatus != string(domain.HoldStatusActive) {
		t.Errorf("回滚后 hold 应仍 active, got %q", holdStatus)
	}

	// Balance unchanged: from=200.00.
	tr := repo.NewTxnRepo(db)
	bf, _ := tr.GetLatestBalance(ctx, from)
	if bf.Balance != domain.NewMoneyFromCents(20000) {
		t.Errorf("回滚后 from 余额=%s, want 200.00", bf.Balance)
	}

	// No idempotency mapping persisted.
	var mappingCount int
	db.QueryRowContext(ctx, "SELECT count(*) FROM held_transfer WHERE idempotency_key=$1", "RBK1").Scan(&mappingCount)
	if mappingCount != 0 {
		t.Errorf("回滚后不应有 held_transfer 记录, got %d", mappingCount)
	}
}

// TestHeldTransfer_Reverse_ImmutableEntries verifies that ReverseTransfer
// creates opposite entries in a new voucher while the original voucher stays
// unchanged, and that a duplicate reversal is rejected.
func TestHeldTransfer_Reverse_ImmutableEntries(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const from, to = "HT-RV1", "HT-RV2"
	setupHeldTransferAccounts(t, db, from, to)
	db.ExecContext(ctx, "DELETE FROM funds_hold WHERE account_no=$1", from)
	db.ExecContext(ctx, "DELETE FROM held_transfer WHERE idempotency_key='RVK1'")
	db.ExecContext(ctx, "DELETE FROM voucher_reversal WHERE reverses_voucher_no LIKE 'V%'")
	db.ExecContext(ctx, "DELETE FROM outbox_message WHERE message_type LIKE 'core.%'")

	hr := repo.NewHoldRepo(db)
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)
	ls := service.NewLedgerService(lr)
	holdSvc := service.NewHoldService(db, hr)
	transferSvc := service.NewHeldTransferService(db, hr, ar, ls, lr, lr)

	// Post a held transfer to get a reversible voucher.
	hold, err := holdSvc.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "PH-RV1", AccountNo: from,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PlaceHold: %v", err)
	}
	posted, err := transferSvc.PostHeldTransfer(ctx, service.PostHeldTransfer{
		IdempotencyKey: "RVK1", HoldID: hold.HoldID,
		FromAccount: from, ToAccount: to,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PostHeldTransfer: %v", err)
	}

	// Balances after post: from=150.00, to=50.00.
	tr := repo.NewTxnRepo(db)
	bf, _ := tr.GetLatestBalance(ctx, from)
	bt, _ := tr.GetLatestBalance(ctx, to)
	if bf.Balance != domain.NewMoneyFromCents(15000) {
		t.Fatalf("post 后 from 余额=%s, want 150.00", bf.Balance)
	}
	if bt.Balance != domain.NewMoneyFromCents(5000) {
		t.Fatalf("post 后 to 余额=%s, want 50.00", bt.Balance)
	}

	// Reverse the held transfer.
	reversed, err := transferSvc.ReverseTransfer(ctx, service.ReverseTransfer{
		IdempotencyKey:    "RK-RV1",
		OriginalVoucherNo: posted.VoucherNo,
	})
	if err != nil {
		t.Fatalf("ReverseTransfer: %v", err)
	}
	if reversed.VoucherNo == posted.VoucherNo {
		t.Fatal("冲正 voucher 应不同")
	}

	// Original entries unchanged: still 2 entries, still status='normal'.
	var origCount, origReversed int
	db.QueryRowContext(ctx,
		"SELECT count(*) FROM acct_txn WHERE voucher_no=$1", posted.VoucherNo).Scan(&origCount)
	db.QueryRowContext(ctx,
		"SELECT count(*) FROM acct_txn WHERE voucher_no=$1 AND txn_status='reversed'", posted.VoucherNo).Scan(&origReversed)
	if origCount != 2 {
		t.Errorf("原始凭证应仍 2 条流水, got %d", origCount)
	}
	if origReversed != 0 {
		t.Errorf("原始凭证状态应仍 normal, got %d reversed", origReversed)
	}

	// Reversal entries: 2 new entries with flipped DC.
	var revCount int
	db.QueryRowContext(ctx,
		"SELECT count(*) FROM acct_txn WHERE voucher_no=$1", reversed.VoucherNo).Scan(&revCount)
	if revCount != 2 {
		t.Errorf("冲正凭证应 2 条流水, got %d", revCount)
	}

	// Balances restored: from=200.00, to=0.00.
	bf2, _ := tr.GetLatestBalance(ctx, from)
	bt2, _ := tr.GetLatestBalance(ctx, to)
	if bf2.Balance != domain.NewMoneyFromCents(20000) {
		t.Errorf("冲正后 from 余额=%s, want 200.00", bf2.Balance)
	}
	if bt2.Balance != domain.NewMoneyFromCents(0) {
		t.Errorf("冲正后 to 余额=%s, want 0.00", bt2.Balance)
	}

	// Duplicate reversal rejected.
	_, err = transferSvc.ReverseTransfer(ctx, service.ReverseTransfer{
		IdempotencyKey:    "RK-RV2",
		OriginalVoucherNo: posted.VoucherNo,
	})
	if !errors.Is(err, service.ErrVoucherAlreadyReversed) {
		t.Fatalf("重复冲正应 ErrVoucherAlreadyReversed, got %v", err)
	}

	// voucher_reversal row exists.
	var revRowCount int
	db.QueryRowContext(ctx,
		"SELECT count(*) FROM voucher_reversal WHERE reverses_voucher_no=$1", posted.VoucherNo).Scan(&revRowCount)
	if revRowCount != 1 {
		t.Errorf("voucher_reversal count=%d, want 1", revRowCount)
	}
}

// Compile-time assertion: outboxFailingLedgerRepo satisfies TransferStore.
var _ service.TransferStore = (*outboxFailingLedgerRepo)(nil)
