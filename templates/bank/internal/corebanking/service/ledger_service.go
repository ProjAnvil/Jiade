// Package service 是 core-banking 用例层：业务规则，纯逻辑可单测。
package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
)

// ErrUnbalanced 借贷不平——复式记账核心不变量被违反。
var ErrUnbalanced = fmt.Errorf("ledger: 借贷不平")

// LedgerStore service 依赖的持久化接口（依赖倒置：repo 实现它）。
type LedgerStore interface {
	InsertTxns(ctx context.Context, txns []domain.Txn) error
	ApplyBalanceDeltas(ctx context.Context, bizDate string, deltas []domain.BalanceDelta) error
	UpsertGL(ctx context.Context, gl domain.GLBalance) error
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

// Post 过账：校验平衡 → 写流水 → 累加分户账余额 → 更新总账。
// 不平则拒绝且绝不调用 store（验收 #7）。
func (s *LedgerService) Post(ctx context.Context, entries []domain.Entry, bizDate, ccy string) error {
	if _, _, err := ValidateBalance(entries); err != nil {
		return err
	}
	txns, deltas, gl := summarize(entries, bizDate, ccy)
	if err := s.store.InsertTxns(ctx, txns); err != nil {
		return fmt.Errorf("ledger: 写流水失败: %w", err)
	}
	if err := s.store.ApplyBalanceDeltas(ctx, bizDate, deltas); err != nil {
		return fmt.Errorf("ledger: 更新分户账失败: %w", err)
	}
	return s.store.UpsertGL(ctx, gl)
}

func summarize(entries []domain.Entry, bizDate, ccy string) ([]domain.Txn, []domain.BalanceDelta, domain.GLBalance) {
	txns := make([]domain.Txn, 0, len(entries))
	byAcct := map[string]domain.Money{}
	subjByAcct := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, e := range entries {
		txns = append(txns, domain.Txn{
			BizDate: bizDate, AccountNo: e.AccountNo, DCFlag: e.DCFlag,
			Amount: e.Amount, Ccy: ccy, SubjectCode: e.SubjectCode,
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
		gl.SubjectCode = deltas[0].SubjectCode // Spec A 单科目过账简化
	}
	return txns, deltas, gl
}
