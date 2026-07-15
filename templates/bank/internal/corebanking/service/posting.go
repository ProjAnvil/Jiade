package service

import (
	"fmt"

	"bank/internal/corebanking/domain"
)

// BuildEntries 把业务动作翻译成复式分录（借一贷一，天然平衡）。
//   deposit  ：借 现金(1001) / 贷 账户科目       — counterparty 不用
//   withdraw ：借 账户科目   / 贷 现金(1001)      — counterparty 不用
//   transfer ：借 账户科目   / 贷 counterparty 科目
// acct 是主账户（存入/支取/转出方）；amount 为分。
func BuildEntries(action domain.Action, acct domain.DemandAccount, counterparty *domain.DemandAccount, amount domain.Money) ([]domain.Entry, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("posting: 金额必须 > 0")
	}
	cashEntry := func(flag domain.DCFlag) domain.Entry {
		return domain.Entry{AccountNo: domain.CashSubject, DCFlag: flag, Amount: amount, SubjectCode: domain.CashSubject}
	}
	acctEntry := func(flag domain.DCFlag) domain.Entry {
		return domain.Entry{AccountNo: acct.AccountNo, DCFlag: flag, Amount: amount, SubjectCode: acct.SubjectCode}
	}
	switch action {
	case domain.ActionDeposit:
		return []domain.Entry{cashEntry(domain.DCDebit), acctEntry(domain.DCCredit)}, nil
	case domain.ActionWithdraw:
		return []domain.Entry{acctEntry(domain.DCDebit), cashEntry(domain.DCCredit)}, nil
	case domain.ActionTransfer:
		if counterparty == nil {
			return nil, fmt.Errorf("posting: transfer 需要 counterparty")
		}
		opp := domain.Entry{AccountNo: counterparty.AccountNo, DCFlag: domain.DCCredit, Amount: amount, SubjectCode: counterparty.SubjectCode}
		return []domain.Entry{acctEntry(domain.DCDebit), opp}, nil
	default:
		return nil, fmt.Errorf("posting: 未知 action %q", action)
	}
}
