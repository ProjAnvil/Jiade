package service

import (
	"testing"

	"bank/internal/corebanking/domain"
)

func acct(no, subj string) domain.DemandAccount {
	return domain.DemandAccount{AccountNo: no, SubjectCode: subj, Ccy: "CNY", Status: domain.AccountStatusActive}
}

func TestBuildEntries_Deposit(t *testing.T) {
	es, err := BuildEntries(domain.ActionDeposit, acct("D1", "2011"), nil, domain.NewMoneyFromCents(10000))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("应 2 条分录, got %d", len(es))
	}
	// 借 1001 现金 / 贷 D1 活期
	if es[0].AccountNo != domain.CashSubject || es[0].DCFlag != domain.DCDebit || es[0].SubjectCode != "1001" {
		t.Errorf("借方分录不对: %+v", es[0])
	}
	if es[1].AccountNo != "D1" || es[1].DCFlag != domain.DCCredit || es[1].SubjectCode != "2011" {
		t.Errorf("贷方分录不对: %+v", es[1])
	}
}

func TestBuildEntries_Withdraw(t *testing.T) {
	es, _ := BuildEntries(domain.ActionWithdraw, acct("D1", "2011"), nil, domain.NewMoneyFromCents(10000))
	// 借 D1 活期 / 贷 1001 现金
	if es[0].AccountNo != "D1" || es[0].DCFlag != domain.DCDebit {
		t.Errorf("借方应 D1 借: %+v", es[0])
	}
	if es[1].AccountNo != domain.CashSubject || es[1].DCFlag != domain.DCCredit {
		t.Errorf("贷方应 1001 贷: %+v", es[1])
	}
}

func TestBuildEntries_Transfer(t *testing.T) {
	to := acct("D2", "2011")
	es, _ := BuildEntries(domain.ActionTransfer, acct("D1", "2011"), &to, domain.NewMoneyFromCents(5000))
	// 借 D1 / 贷 D2
	if es[0].AccountNo != "D1" || es[0].DCFlag != domain.DCDebit {
		t.Errorf("借方应 D1: %+v", es[0])
	}
	if es[1].AccountNo != "D2" || es[1].DCFlag != domain.DCCredit {
		t.Errorf("贷方应 D2: %+v", es[1])
	}
}

func TestBuildEntries_TransferMissingCounterparty(t *testing.T) {
	if _, err := BuildEntries(domain.ActionTransfer, acct("D1", "2011"), nil, 100); err == nil {
		t.Error("transfer 缺 counterparty 应报错")
	}
}

func TestBuildEntries_UnknownAction(t *testing.T) {
	if _, err := BuildEntries(domain.Action("loan"), acct("D1", "2011"), nil, 100); err == nil {
		t.Error("未知 action 应报错")
	}
}
