package service

import (
	"fmt"

	"bank/internal/corebanking/domain"
)

// BuildEntries translate business actions into double-entry entries (debit one, credit one, natural balance).
// deposit: debit cash (1001) / credit account account — counterparty is not used
// withdraw: debit account/credit cash (1001) — counterparty not required
// transfer: debit account account / loan counterparty account
// acct is the main account (deposit/withdrawal/transfer party); amount is points.
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
