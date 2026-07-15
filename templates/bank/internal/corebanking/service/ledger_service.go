// Package service 是 core-banking 用例层：业务规则，纯逻辑可单测。
package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// ErrUnbalanced 借贷不平——复式记账核心不变量被违反。
var ErrUnbalanced = fmt.Errorf("ledger: 借贷不平")

// LedgerStore service 依赖的持久化接口（依赖倒置：repo 实现它）。
type LedgerStore interface {
	InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error
	ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error
	UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error
	EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error)
	GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
	// LockTxnsByVoucher 冲正专用：SELECT ... FOR UPDATE 锁本凭证流水行（串行化并发冲正）。
	LockTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
	// HasReversal 红冲去重：是否已有 ref_txn_id=$refTxnID 的反向分录。
	HasReversal(ctx context.Context, q pg.DBTX, refTxnID string) (bool, error)
	UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error
	SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error
}

// LedgerService 复式记账用例。
type LedgerService struct {
	store LedgerStore
}

func NewLedgerService(store LedgerStore) *LedgerService {
	return &LedgerService{store: store}
}

// ValidateBalance 校验复式记账平衡，返回借/贷合计。不平返回 ErrUnbalanced。
// 纯函数，无副作用，是验收 #7 的核心。
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

// Post 过账：校验平衡 → 汇总分录 → 写流水 → 累加分户账余额 → 更新总账。
// q 为事务（或连接池）执行器，调用方负责事务边界（见 txn_service）。
// voucherNo 标记本笔凭证；refTxnID 非空表示本笔是冲正（关联原流水）。
// 不平则拒绝且绝不调用 store（验收 #7）。返回生成的流水（含回填的 TxnID）。
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
