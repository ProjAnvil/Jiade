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
	// 建一个历史日余额行（基线 500.00 元；列为 numeric(18,2)，以「元」存值）
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ('IT-D3','2026-07-15',500.00,500.00,'2011')`)

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
		// 列为 numeric(18,2) 以「元」存值；10000.00 元 = 1000000 分（断言以分为单位）。
		db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
			VALUES ($1,'2026-07-15',10000.00,10000.00,'2011')`, no) // 各 10000.00 元
	}

	// 本测试日期假设独立于 seed 契约：Record 按 sys_param.biz_date 写当日快照，
	// 须晚于初始余额日 2026-07-15。测后恢复，免污染同库其他测试。
	var prevBizDate string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&prevBizDate); err != nil {
		t.Fatalf("读 biz_date: %v", err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE sys_param SET param_value='2026-07-16' WHERE param_key='biz_date'"); err != nil {
		t.Fatalf("调 biz_date: %v", err)
	}
	defer db.ExecContext(context.Background(), "UPDATE sys_param SET param_value=$1 WHERE param_key='biz_date'", prevBizDate)

	errs := make(chan error, 2)
	// T1: A→B；T2: B→A —— 若无 lock ordering 会 AB-BA 死锁
	go func() { _, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-A", ToAccount: "CD-B", Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY"}); errs <- e }()
	go func() { _, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-B", ToAccount: "CD-A", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY"}); errs <- e }()

	for i := 0; i < 2; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("并发转账失败: %v", e)
		}
	}
	// 两笔都成功：A 余额 10000-100+50=9950.00；B 余额 10000+100-50=10050.00
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

// TestReverse_Concurrent_DuplicateRejected 验证 B-3 final review Important #1 修复：
// 两个并发蓝冲同一凭证，必须只有一个成功，另一个返回 ErrAlreadyReversed；
// 且余额只回滚一次（不是两次——否则资金会被凭空创造）。
//
// 修复前：GetTxnsByVoucher 无 FOR UPDATE、UpdateTxnStatus 无 normal 守卫 →
// 两个并发蓝冲都过「未 reversed」检查 → 双回滚 → 余额被多回滚一次 = 资金漏洞。
func TestReverse_Concurrent_DuplicateRejected(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	const acct = "RV-A"
	// 清理历史
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	// 建一个历史日余额行（基线 0.00 元）—— EnsureBalanceRow 须有历史行才能继承到当天。
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)

	// 先存入 100.00（10000 分）得到一个可冲正的凭证
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// 读取冲正前余额（当天，应该 = 100.00 元）
	tr := repo.NewTxnRepo(db)
	balBefore, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("冲正前 %s 余额=%s（基线）", acct, balBefore.Balance)

	// 并发两个蓝冲同一凭证
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

	// 必须恰好一个成功、一个 ErrAlreadyReversed
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

	// 关键资金安全断言：余额只回滚一次（回到 0），而非回滚两次（变 -100.00 = -10000 分）。
	balAfter, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// 原存 100.00 → 蓝冲回滚一次应回到 0。若双回滚则为 -100.00。
	if balAfter.Balance != domain.NewMoneyFromCents(0) {
		t.Errorf("蓝冲后余额=%s, want 0（回滚一次）；若为负值说明双回滚=资金漏洞", balAfter.Balance)
	}
	t.Logf("冲正后 %s 余额=%s（应为 0）", acct, balAfter.Balance)
}

// TestReverse_BlueThenRed_SecondRejected 验证：先蓝冲成功后，再红冲同凭证应被拒绝。
// 蓝冲把 txn_status 改为 reversed，红冲进入后 LockTxnsByVoucher 读到的行已是 reversed →
// 走「any TxnStatus==reversed → ErrAlreadyReversed」分支。
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

	// 先蓝冲（应成功）
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseBlue); err != nil {
		t.Fatalf("先蓝冲应成功: %v", err)
	}
	// 再红冲（凭证已 reversed，应拒绝）
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("蓝冲后红冲同凭证应 ErrAlreadyReversed, got %v", err)
	}
}

// TestReverse_RedThenRed_SecondRejected 验证：先红冲成功后，再红冲同凭证应被拒绝。
// 红冲不改 txn_status（spec §7.3），所以靠 HasReversal(ref_txn_id) 去重：
// 首笔红冲 Post 的反向分录带 ref_txn_id 指向原流水；次笔 LockTxnsByVoucher 串行后
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

	// 先红冲（应成功）
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); err != nil {
		t.Fatalf("先红冲应成功: %v", err)
	}
	// 再红冲同凭证（HasReversal=true，应拒绝）
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("红冲后红冲同凭证应 ErrAlreadyReversed, got %v", err)
	}
}

// TestReverse_RedThenBlue_SecondRejected 验证 B-3 fix2：先红冲成功后，再蓝冲同凭证应被拒绝，
// 且余额只回滚一次（不是两次）。
//
// 修复前：红冲不改 txn_status（spec §7.3，原流水仍 normal），蓝冲分支只靠 UpdateTxnStatus 的
// normal 守卫去重——首笔红冲后 normal 守卫仍能匹配 → UpdateTxnStatus 成功 → reverseRollback
// 再回滚一次 → 双倍回滚 = 资金凭空创造。蓝冲分支在 UpdateTxnStatus 前新增 HasReversal(origs[0].TxnID)
// 检查：红冲已落 ref_txn_id 反向分录 → HasReversal=true → ErrAlreadyReversed，拒绝蓝冲。
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
	// 基线 0.00 元
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',0.00,0.00,'2011')`, acct)
	// 存入 100.00（10000 分）得到可冲正凭证
	booking, err := svc.Record(ctx, service.RecordInput{
		Action: domain.ActionDeposit, AccountNo: acct,
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("setup Record 失败: %v", err)
	}
	voucher := booking.VoucherNo

	// 读取红冲前余额（应 = 100.00 元）
	tr := repo.NewTxnRepo(db)
	balAfterRed, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("红冲前 %s 余额=%s（应为 100.00）", acct, balAfterRed.Balance)

	// 先红冲（应成功：原流水 normal 不变，反向分录入账，余额回到 0）
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseRed); err != nil {
		t.Fatalf("先红冲应成功: %v", err)
	}
	balAfterRed, err = tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// 红冲反向分录把 100.00 抵消 → 余额回到 0
	if balAfterRed.Balance != domain.NewMoneyFromCents(0) {
		t.Fatalf("红冲后余额=%s, want 0（红冲一次回滚后）", balAfterRed.Balance)
	}
	t.Logf("红冲后 %s 余额=%s（应为 0）", acct, balAfterRed.Balance)

	// 再蓝冲同凭证：HasReversal 应为 true → ErrAlreadyReversed
	if _, err := svc.Reverse(ctx, voucher, domain.ReverseBlue); !errors.Is(err, service.ErrAlreadyReversed) {
		t.Fatalf("红冲后蓝冲同凭证应 ErrAlreadyReversed, got %v", err)
	}

	// 关键资金安全断言：余额仍为 0（只回滚一次），而非 -100.00（蓝冲再回滚一次 = 双倍回滚）。
	balFinal, err := tr.GetLatestBalance(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if balFinal.Balance != domain.NewMoneyFromCents(0) {
		t.Errorf("红后蓝后余额=%s, want 0（蓝冲被拒，余额不变）；若为负值说明蓝冲双倍回滚=资金漏洞", balFinal.Balance)
	}
	t.Logf("红后蓝后 %s 余额=%s（应为 0，蓝冲被拒）", acct, balFinal.Balance)
}

// 编译期断言：确保 *sql.DB 不需要的 var 被引用（避免 unused import）。
var _ = pg.RunInTx
