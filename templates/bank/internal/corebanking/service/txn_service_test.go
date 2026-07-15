package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
)

// fakeAccountsRdr 记账用的账户只读接口 fake。
type fakeAccountsRdr struct {
	byNo map[string]domain.DemandAccount
}

func (f fakeAccountsRdr) GetDemand(_ context.Context, no string) (domain.DemandAccount, error) {
	if a, ok := f.byNo[no]; ok {
		return a, nil
	}
	return domain.DemandAccount{}, errNotFound{}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

func TestRecord_Deposit_Success(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)

	booking, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1", Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("deposit 应成功: %v", err)
	}
	if booking.VoucherNo == "" || len(booking.Txns) != 2 {
		t.Errorf("应返回 voucherNo + 2 条流水, got %+v", booking)
	}
	// deposit 应写入两条复式流水（借现金/贷账户），且落 summary
	if booking.Txns[0].VoucherNo != booking.VoucherNo {
		t.Errorf("流水 voucherNo 不匹配: got %s want %s", booking.Txns[0].VoucherNo, booking.VoucherNo)
	}
}

func TestRecord_Deposit_WithSummary_CallsSetTxnSummary(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)

	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1",
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
		Summary: "工资入账",
	})
	if err != nil {
		t.Fatalf("deposit+summary 应成功: %v", err)
	}
	if store.summaryCalls == 0 {
		t.Errorf("Summary 非空时必须调用 SetTxnSummary（落库），summaryCalls=%d", store.summaryCalls)
	}
	if store.lastSummary != "工资入账" {
		t.Errorf("SetTxnSummary 收到的 summary=%q want %q", store.lastSummary, "工资入账")
	}
	if store.lastSummaryVoucher == "" {
		t.Errorf("SetTxnSummary 应收到非空 voucherNo")
	}
}

func TestRecord_Deposit_NoSummary_SkipsSetTxnSummary(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)

	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1",
		Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("deposit 无 summary 应成功: %v", err)
	}
	if store.summaryCalls != 0 {
		t.Errorf("Summary 为空时不应调用 SetTxnSummary, summaryCalls=%d", store.summaryCalls)
	}
}

func TestRecord_AccountNotFound(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "NOPE", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("账户不存在应 ErrAccountNotFound, got %v", err)
	}
}

func TestRecord_AccountNotActive(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusFrozen},
	}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionWithdraw, AccountNo: "D1", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("冻结账户应 ErrAccountNotActive, got %v", err)
	}
}

func TestRecord_CcyMismatch(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1",
		Amount: domain.NewMoneyFromCents(100), Ccy: "USD",
	})
	if !errors.Is(err, ErrCcyMismatch) {
		t.Fatalf("币种不一致应 ErrCcyMismatch, got %v", err)
	}
}

func TestLockedAccountList_SingleAccount(t *testing.T) {
	in := RecordInput{Action: domain.ActionDeposit, AccountNo: "D1"}
	got := lockedAccountList(in)
	if len(got) != 1 || got[0] != "D1" {
		t.Errorf("单账户 lock list = %v, want [D1]", got)
	}
}

func TestLockedAccountList_Transfer_AscendingOrder(t *testing.T) {
	// transfer：from > to，应返回升序 [T1, T2]
	in := RecordInput{Action: domain.ActionTransfer, FromAccount: "T2", ToAccount: "T1"}
	got := lockedAccountList(in)
	if len(got) != 2 {
		t.Fatalf("transfer lock list 长度 = %d, want 2", len(got))
	}
	if got[0] != "T1" || got[1] != "T2" {
		t.Errorf("transfer lock list 应升序, got %v want [T1 T2]", got)
	}
}

func TestLockedAccountList_Transfer_AlreadyAscending(t *testing.T) {
	in := RecordInput{Action: domain.ActionTransfer, FromAccount: "A1", ToAccount: "A2"}
	got := lockedAccountList(in)
	if got[0] != "A1" || got[1] != "A2" {
		t.Errorf("已升序的 transfer lock list 应保持, got %v want [A1 A2]", got)
	}
}
