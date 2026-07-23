package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// Bookkeeping/reversal errors.
var (
	ErrAccountNotFound     = fmt.Errorf("账户不存在")
	ErrAccountNotActive    = fmt.Errorf("账户非 active 状态")
	ErrInsufficientBalance = fmt.Errorf("余额不足")
	ErrCcyMismatch         = fmt.Errorf("币种不一致")
	ErrVoucherNotFound     = fmt.Errorf("凭证不存在")
	ErrAlreadyReversed     = fmt.Errorf("凭证已冲正")
)

// AccountReader Account read-only query for accounting (implemented by repo.AccountRepo).
type AccountReader interface {
	GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error)
}

// TxnStore flow/balance query interface (read-only, repo implementation) - retains the original read-only capability.
type TxnStore interface {
	ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error)
	GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error)
}

type TxnService struct {
	db       *sql.DB
	accounts AccountReader
	ledger   *LedgerService
	store    LedgerStore
	read     TxnStore // Optional: read-only query inheritance (decoupled from write dependencies)
}

// NewTxnService constructs an accounting service: db is the transaction boundary; accounts read accounts; ledger posts; store stores (including SetTxnSummary).
func NewTxnService(db *sql.DB, accounts AccountReader, ledger *LedgerService, store LedgerStore) *TxnService {
	return &TxnService{db: db, accounts: accounts, ledger: ledger, store: store}
}

// WithReader injects the read-only store (ListTxns/GetBalance is reused by the api read-only handler).
func (s *TxnService) WithReader(read TxnStore) *TxnService { s.read = read; return s }

// RecordInput accounting request. Use AccountNo for deposit/withdraw; use FromAccount/ToAccount for transfer.
type RecordInput struct {
	Action                            domain.Action
	AccountNo, FromAccount, ToAccount string
	Amount                            domain.Money
	Ccy, Summary                      string
}

// Record accounting: business intent → double entry → atomic posting within transaction.
// Within the transaction: Lock account balance row (EnsureBalanceRow) → Verify (active/ccy/overdraft) → BuildEntries → Post → drop summary.
// transfer locks two accounts in ascending order of account_no (to prevent AB-BA deadlock).
// Return domain.Booking(voucherNo + duplex voucher).
// Production path db non-nil → pg.RunInTx package; db==nil (only fake store for unit testing) → execute fn directly with q=nil.
func (s *TxnService) Record(ctx context.Context, in RecordInput) (domain.Booking, error) {
	bizDate, err := s.store.GetBizDate(ctx)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("txn: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return domain.Booking{}, fmt.Errorf("txn: sys_param.biz_date 未设置，请先 seed")
	}
	voucherNo := domain.NewVoucherNo(bizDate)
	var booking domain.Booking

	run := pg.RunInTx
	if s.db == nil {
		// Single test path: fake store ignores q and directly executes fn (transaction atomicity is covered by integration tests)
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err = run(ctx, s.db, func(q pg.DBTX) error {
		// Parse the main account (single acquisition to avoid redundant secondary queries)
		primaryNo := in.AccountNo
		if in.Action == domain.ActionTransfer {
			primaryNo = in.FromAccount
		}
		acct, err := s.accounts.GetDemand(ctx, primaryNo)
		if err != nil {
			return ErrAccountNotFound
		}
		if acct.Status != domain.AccountStatusActive {
			return ErrAccountNotActive
		}
		if in.Ccy != "" && in.Ccy != acct.Ccy {
			return ErrCcyMismatch
		}
		ccy := acct.Ccy

		// Transfer takes the counterparty account (credit)
		var counterparty *domain.DemandAccount
		if in.Action == domain.ActionTransfer {
			toAcct, err := s.accounts.GetDemand(ctx, in.ToAccount)
			if err != nil {
				return ErrAccountNotFound
			}
			if toAcct.Status != domain.AccountStatusActive {
				return ErrAccountNotActive
			}
			counterparty = &toAcct
		}

		entries, err := BuildEntries(in.Action, acct, counterparty, in.Amount)
		if err != nil {
			return err
		}

		// Lock account balance rows (in ascending order by account_no to prevent AB-BA deadlock) + overdraft check
		// Overdraft: If the payer (acct.AccountNo) of withdraw/transfer has insufficient balance, it will be rejected.
		lockAccounts := lockedAccountList(in)
		for _, no := range lockAccounts {
			subject := acct.SubjectCode
			if in.Action == domain.ActionTransfer && no == in.ToAccount && counterparty != nil {
				subject = counterparty.SubjectCode
			}
			bal, err := s.store.EnsureBalanceRow(ctx, q, no, bizDate, subject)
			if err != nil {
				return err
			}
			// Only do overdraft checks on the payer (acct.AccountNo). deposit is not checked.
			if no == acct.AccountNo && (in.Action == domain.ActionWithdraw || in.Action == domain.ActionTransfer) {
				if in.Amount > bal.AvailableBalance {
					return ErrInsufficientBalance
				}
			}
		}

		// Post (Post does not fill in summary, summary will be stored separately in UPDATE after Post)
		txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, voucherNo, "")
		if err != nil {
			return err
		}
		// summary drop database (DB UPDATE, non-memory assignment)
		if in.Summary != "" {
			if err := s.store.SetTxnSummary(ctx, q, voucherNo, in.Summary); err != nil {
				return err
			}
			for i := range txns {
				txns[i].Summary = in.Summary
			}
		}
		booking = domain.Booking{VoucherNo: voucherNo, BizDate: bizDate, Txns: txns}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}
	return booking, nil
}

// ReverseResult Reverse result.
type ReverseResult struct {
	VoucherNo         string
	Mode              string
	Status            string // Blue rush: reversed; red rush: original normal (unchanged)
	ReversedVoucherNo string // The reverse voucher number generated by the red flush; the blue flush is empty.
	Txns              []domain.Txn
}

// Reverse: blue = change status + reverse delta rollback (no new transaction will be added); red = reverse entry will be Posted (reverse transaction will be added).
// One voucher can only be used once. The correction itself cannot be corrected again.
//
// Concurrency safety (final review Important #1 fix):
// - First use LockTxnsByVoucher (SELECT ... FOR UPDATE) within the transaction to lock all pipeline lines of this voucher and serialize it
// Concurrent reversal of the same voucher. Two concurrent reverses are queued here, and the first one has been committed when the second one gets the lock.
// - Blue rush: first intercept "red and then blue" with HasReversal (red rush does not change txn_status, normal guard cannot stop it);
// Then UpdateTxnStatus (WHERE with txn_status='normal' guard) blocks "blue-blue concurrency". The first stroke is normal→reversed
// After submission, only reversed rows are left to match for this UPDATE → RowsAffected=0 → sql.ErrNoRows →
// ErrAlreadyReversed. Even ignoring locks, these two layers of guards are enough to prevent double rollbacks.
// - Red flush: txn_status does not change (spec §7.3 Red flush original running water txn_status remains unchanged). Use HasReversal(ref_txn_id) instead
// Determine whether the first red flush reverse entry has been deposited; the reverse entries submitted by the first Post have ref_txn_id, and the second one is locked
// After HasReversal=true → ErrAlreadyReversed.
//
// The four flush sequence combinations all have guards (B-3 fix2 closes "red and then blue"):
// - Lanlan: UpdateTxnStatus normal guard rejects the second pen ✓
// - Blue-red: `any TxnStatus==reversed` check rejected ✓
// - Red: HasReversal rejected ✓
// - Red and Blue: New HasReversal rejection for Blue Chong entrance ✓
//
// Production path db non-nil → pg.RunInTx package; db==nil (single test) → execute fn directly with q=nil.
func (s *TxnService) Reverse(ctx context.Context, voucherNo string, mode domain.ReverseMode) (ReverseResult, error) {
	bizDate, err := s.store.GetBizDate(ctx)
	if err != nil {
		return ReverseResult{}, fmt.Errorf("txn: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return ReverseResult{}, fmt.Errorf("txn: sys_param.biz_date 未设置，请先 seed")
	}
	var res ReverseResult

	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err = run(ctx, s.db, func(q pg.DBTX) error {
		// Lock all pipeline lines of this certificate (FOR UPDATE), serialize and correct concurrently.
		origs, err := s.store.LockTxnsByVoucher(ctx, q, voucherNo)
		if err != nil {
			return err
		}
		if len(origs) == 0 {
			return ErrVoucherNotFound
		}
		// Anti-duplication: Any flow of the original certificate has been reversed → rejected (this check is within the row lock and is effective for concurrent blue flushes).
		for _, t := range origs {
			if t.TxnStatus == domain.TxnStatusReversed {
				return ErrAlreadyReversed
			}
		}
		ccy := origs[0].Ccy

		switch mode {
		case domain.ReverseBlue:
			// Red rush does not change txn_status (spec §7.3), normal guard cannot stop "red behind blue" - use HasReversal to cover up.
			// Blue-blue concurrency is guarded by the following UpdateTxnStatus normal; red-blue/red-red is guarded by HasReversal.
			if has, err := s.store.HasReversal(ctx, q, origs[0].TxnID); err != nil {
				return err
			} else if has {
				return ErrAlreadyReversed
			}
			// Atomic normal→reversed guard: only one of the concurrent blue rushes is changed to the line.
			if err := s.store.UpdateTxnStatus(ctx, q, voucherNo, domain.TxnStatusReversed); err != nil {
				// RowsAffected=0 (all rows reversed) → sql.ErrNoRows → treated as reversed.
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAlreadyReversed
				}
				return err
			}
			// Reverse delta rollback balance/general ledger (mirror of original delta: credit → debit flip symbol)
			deltas, gl := reverseRollback(origs, bizDate)
			if err := s.store.ApplyBalanceDeltas(ctx, q, bizDate, deltas); err != nil {
				return err
			}
			if err := s.store.UpsertGL(ctx, q, gl); err != nil {
				return err
			}
			res = ReverseResult{VoucherNo: voucherNo, Mode: string(mode), Status: string(domain.TxnStatusReversed)}

		case domain.ReverseRed:
			// The red flush does not change txn_status (spec §7.3); use HasReversal to determine whether the first red flush reverse entry has been dropped.
			ref := origs[0].TxnID
			hasRev, err := s.store.HasReversal(ctx, q, ref)
			if err != nil {
				return err
			}
			if hasRev {
				return ErrAlreadyReversed
			}
			newVoucher := domain.NewVoucherNo(bizDate)
			entries := reverseEntries(origs)
			txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, newVoucher, ref)
			if err != nil {
				return err
			}
			res = ReverseResult{VoucherNo: voucherNo, Mode: string(mode), Status: string(domain.TxnStatusNormal),
				ReversedVoucherNo: newVoucher, Txns: txns}

		default:
			return fmt.Errorf("txn: 未知冲正模式 %q", mode)
		}
		return nil
	})
	if err != nil {
		return ReverseResult{}, err
	}
	return res, nil
}

// reverseRollback calculates the reverse BalanceDelta from the original flow (original loan + → reverse -, original debit - → reverse +) and the mirrored general ledger.
// Note that the symbolic semantics of byAcct (net value) and gl (balance) are different: Sub is always used for balance rollback.
func reverseRollback(txns []domain.Txn, bizDate string) ([]domain.BalanceDelta, domain.GLBalance) {
	byAcct := map[string]domain.Money{}
	subj := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, t := range txns {
		if t.DCFlag == domain.DCCredit {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Sub(t.Amount) // Original loan + → Reverse -
			glCC = glCC.Sub(t.Amount)                               // Balance rollback: minus
		} else {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Add(t.Amount) // Original borrow- → reverse +
			glDC = glDC.Sub(t.Amount)                               // Balance rollback: Subtract (not Add!)
		}
		subj[t.AccountNo] = t.SubjectCode
	}
	deltas := make([]domain.BalanceDelta, 0, len(byAcct))
	for acct, d := range byAcct {
		deltas = append(deltas, domain.BalanceDelta{AccountNo: acct, Delta: d, SubjectCode: subj[acct]})
	}
	gl := domain.GLBalance{BizDate: bizDate, DCBalance: glDC, CCBalance: glCC, Ccy: txns[0].Ccy}
	if len(deltas) > 0 {
		gl.SubjectCode = deltas[0].SubjectCode
	}
	return deltas, gl
}

// reverseEntries is a reverse entry constructed from the original flow structure (dc_flag is flipped, the amount remains unchanged) - used for red flushing and Post.
func reverseEntries(txns []domain.Txn) []domain.Entry {
	es := make([]domain.Entry, 0, len(txns))
	for _, t := range txns {
		flag := domain.DCCredit
		if t.DCFlag == domain.DCCredit {
			flag = domain.DCDebit
		}
		es = append(es, domain.Entry{AccountNo: t.AccountNo, DCFlag: flag, Amount: t.Amount, SubjectCode: t.SubjectCode})
	}
	return es
}

// lockedAccountList returns a list of accounts to be locked in ascending order by account_no (to prevent AB-BA deadlock).
// deposit/withdraw: AccountNo only. transfer: FromAccount+ToAccount.
func lockedAccountList(in RecordInput) []string {
	var list []string
	if in.Action == domain.ActionTransfer {
		list = []string{in.FromAccount, in.ToAccount}
	} else {
		list = []string{in.AccountNo}
	}
	sort.Strings(list)
	return list
}

// --- Read-only (retain the original capabilities of Spec A for reuse by the api handler) ---

// ListTxns Check the running water (from/to is YYYY-MM-DD, empty means no limit).
func (s *TxnService) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	if s.read == nil {
		return nil, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.ListTxns(ctx, accountNo, from, to)
}

// GetBalance gets the account balance of the latest biz_date.
func (s *TxnService) GetBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	if s.read == nil {
		return domain.Balance{}, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.GetLatestBalance(ctx, accountNo)
}
