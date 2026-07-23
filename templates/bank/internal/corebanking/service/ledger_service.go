// Package service is the core-banking use case layer: business rules, pure logic can be tested individually.
package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// ErrUnbalanced - The core invariants of double-entry accounting are violated.
var ErrUnbalanced = fmt.Errorf("ledger: 借贷不平")

// The persistence interface that the LedgerStore service depends on (dependency inversion: repo implements it).
type LedgerStore interface {
	InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error
	ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error
	UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error
	EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error)
	GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
	// LockTxnsByVoucher is specially used for reversal: SELECT ... FOR UPDATE to lock the voucher (serialized concurrent reversal).
	LockTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
	// HasReversal Red flush to remove duplicates: Whether there is a reverse entry for ref_txn_id=$refTxnID.
	HasReversal(ctx context.Context, q pg.DBTX, refTxnID string) (bool, error)
	UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error
	SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error
	// GetBizDate reads sys_param.biz_date (B-2: Accounting biz_date source).
	GetBizDate(ctx context.Context) (string, error)
}

// LedgerService double-entry accounting use case.
type LedgerService struct {
	store LedgerStore
}

func NewLedgerService(store LedgerStore) *LedgerService {
	return &LedgerService{store: store}
}

// ValidateBalance verifies the double-entry accounting balance and returns the debit/credit total. Unbalanced returns ErrUnbalanced.
// Pure functions, with no side effects, are the core of Acceptance #7.
func ValidateBalance(entries []domain.Entry) (debit, credit domain.Money, err error) {
	for _, e := range entries {
		switch e.DCFlag {
		case domain.DCDebit:
			debit = debit.Add(e.Amount)
		case domain.DCCredit:
			credit = credit.Add(e.Amount)
		default:
			return 0, 0, fmt.Errorf("ledger: 非法借贷标志 %q", e.DCFlag)
		}
	}
	if debit != credit {
		return debit, credit, fmt.Errorf("%w: 借=%s 贷=%s", ErrUnbalanced, debit, credit)
	}
	return debit, credit, nil
}

// Post posting: check balance → summary entries → write journal entries → accumulate sub-account balances → update the general ledger.
// q is the transaction (or connection pool) executor, and the caller is responsible for transaction boundaries (see txn_service).
// voucherNo marks this voucher; refTxnID non-empty indicates that this transaction is corrected (associated with the original transaction).
// If not, reject and never call store (Acceptance #7). Returns the generated pipeline (including backfilled TxnID).
func (s *LedgerService) Post(ctx context.Context, q pg.DBTX, entries []domain.Entry, bizDate, ccy, voucherNo, refTxnID string) ([]domain.Txn, error) {
	if _, _, err := ValidateBalance(entries); err != nil {
		return nil, err
	}
	txns, deltas, gl := summarize(entries, bizDate, ccy, voucherNo, refTxnID)
	if err := s.store.InsertTxns(ctx, q, txns); err != nil {
		return nil, fmt.Errorf("ledger: 写流水失败: %w", err)
	}
	if err := s.store.ApplyBalanceDeltas(ctx, q, bizDate, deltas); err != nil {
		return nil, fmt.Errorf("ledger: 更新分户账失败: %w", err)
	}
	if err := s.store.UpsertGL(ctx, q, gl); err != nil {
		return nil, fmt.Errorf("ledger: 更新总账失败: %w", err)
	}
	return txns, nil
}

func summarize(entries []domain.Entry, bizDate, ccy, voucherNo, refTxnID string) ([]domain.Txn, []domain.BalanceDelta, domain.GLBalance) {
	txns := make([]domain.Txn, 0, len(entries))
	byAcct := map[string]domain.Money{}
	subjByAcct := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, e := range entries {
		txns = append(txns, domain.Txn{
			BizDate: bizDate, AccountNo: e.AccountNo, DCFlag: e.DCFlag,
			Amount: e.Amount, Ccy: ccy, SubjectCode: e.SubjectCode,
			VoucherNo: voucherNo, RefTxnID: refTxnID, TxnStatus: domain.TxnStatusNormal,
		})
		if e.DCFlag == domain.DCCredit {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Add(e.Amount)
			glCC = glCC.Add(e.Amount)
		} else {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Sub(e.Amount)
			glDC = glDC.Add(e.Amount)
		}
		subjByAcct[e.AccountNo] = e.SubjectCode
	}
	deltas := make([]domain.BalanceDelta, 0, len(byAcct))
	for acct, d := range byAcct {
		deltas = append(deltas, domain.BalanceDelta{AccountNo: acct, Delta: d, SubjectCode: subjByAcct[acct]})
	}
	gl := domain.GLBalance{BizDate: bizDate, DCBalance: glDC, CCBalance: glCC, Ccy: ccy}
	if len(deltas) > 0 {
		gl.SubjectCode = deltas[0].SubjectCode
	}
	return txns, deltas, gl
}
