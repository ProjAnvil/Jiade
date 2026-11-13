package service

import (
	"context"
	"errors"
	"testing"

	"bank/core-banking/corebanking/domain"
)

// fakeAccountsRdr Account read-only interface fake for accounting.
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
		t.Fatalf("deposit should succeed: %v", err)
	}
	if booking.VoucherNo == "" || len(booking.Txns) != 2 {
		t.Errorf("should return voucherNo + 2 txns, got %+v", booking)
	}
	// Deposit should be written into two double-entry statements (debit cash/loan account), and fall under summary
	if booking.Txns[0].VoucherNo != booking.VoucherNo {
		t.Errorf("txn voucherNo mismatch: got %s want %s", booking.Txns[0].VoucherNo, booking.VoucherNo)
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
		Summary: "payroll deposit",
	})
	if err != nil {
		t.Fatalf("deposit+summary should succeed: %v", err)
	}
	if store.summaryCalls == 0 {
		t.Errorf("SetTxnSummary must be called when Summary non-empty (persisted), summaryCalls=%d", store.summaryCalls)
	}
	if store.lastSummary != "payroll deposit" {
		t.Errorf("SetTxnSummary received summary=%q want %q", store.lastSummary, "payroll deposit")
	}
	if store.lastSummaryVoucher == "" {
		t.Errorf("SetTxnSummary should receive non-empty voucherNo")
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
		t.Fatalf("deposit without summary should succeed: %v", err)
	}
	if store.summaryCalls != 0 {
		t.Errorf("SetTxnSummary should not be called when Summary empty, summaryCalls=%d", store.summaryCalls)
	}
}

func TestRecord_AccountNotFound(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "NOPE", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("account not found should give ErrAccountNotFound, got %v", err)
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
		t.Fatalf("frozen account should give ErrAccountNotActive, got %v", err)
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
		t.Fatalf("currency mismatch should give ErrCcyMismatch, got %v", err)
	}
}

func TestLockedAccountList_SingleAccount(t *testing.T) {
	in := RecordInput{Action: domain.ActionDeposit, AccountNo: "D1"}
	got := lockedAccountList(in)
	if len(got) != 1 || got[0] != "D1" {
		t.Errorf("single-account lock list = %v, want [D1]", got)
	}
}

func TestLockedAccountList_Transfer_AscendingOrder(t *testing.T) {
	// transfer: from > to, should return ascending order [T1, T2]
	in := RecordInput{Action: domain.ActionTransfer, FromAccount: "T2", ToAccount: "T1"}
	got := lockedAccountList(in)
	if len(got) != 2 {
		t.Fatalf("transfer lock list length = %d, want 2", len(got))
	}
	if got[0] != "T1" || got[1] != "T2" {
		t.Errorf("transfer lock list should be ascending, got %v want [T1 T2]", got)
	}
}

func TestLockedAccountList_Transfer_AlreadyAscending(t *testing.T) {
	in := RecordInput{Action: domain.ActionTransfer, FromAccount: "A1", ToAccount: "A2"}
	got := lockedAccountList(in)
	if got[0] != "A1" || got[1] != "A2" {
		t.Errorf("already-ascending transfer lock list should be kept, got %v want [A1 A2]", got)
	}
}

func TestReverse_Blue_ReversesStatusAndRollbackDeltas(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "1001", VoucherNo: "V1", Ccy: "CNY"},
		{TxnID: "T2", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "2011", VoucherNo: "V1", Ccy: "CNY"},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)

	res, err := svc.Reverse(context.Background(), "V1", domain.ReverseBlue)
	if err != nil {
		t.Fatalf("blue reversal should succeed: %v", err)
	}
	if res.Mode != "blue" || res.Status != string(domain.TxnStatusReversed) {
		t.Errorf("blue reversal result wrong: %+v", res)
	}
	if len(res.Txns) != 0 {
		t.Errorf("blue reversal should not produce new txns, got %d", len(res.Txns))
	}
	if len(store.statusLog) == 0 || store.statusLog[0] != "reversed" {
		t.Errorf("should UpdateTxnStatus=reversed, got %v", store.statusLog)
	}
	if len(store.deltas) == 0 {
		t.Error("blue reversal should roll back delta")
	}
}

func TestReverse_Red_PostsReverseEntries(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "1001", VoucherNo: "V1", Ccy: "CNY"},
		{TxnID: "T2", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "2011", VoucherNo: "V1", Ccy: "CNY"},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)

	res, err := svc.Reverse(context.Background(), "V1", domain.ReverseRed)
	if err != nil {
		t.Fatalf("red reversal should succeed: %v", err)
	}
	if res.Mode != "red" || res.ReversedVoucherNo == "" {
		t.Errorf("red reversal should have new voucher: %+v", res)
	}
	// store.txns contains the original voucherTxns (fake is not real) + the reverse generated by Post 2 items
	if len(store.txns) < 2 {
		t.Errorf("red reversal should produce reverse entries via Post, store.txns=%d", len(store.txns))
	}
}

func TestReverse_AlreadyReversed(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001", VoucherNo: "V1", Ccy: "CNY", TxnStatus: domain.TxnStatusReversed},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)
	_, err := svc.Reverse(context.Background(), "V1", domain.ReverseBlue)
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("already reversed should give ErrAlreadyReversed, got %v", err)
	}
}

func TestReverse_NotFound(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: nil} // Empty voucher
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)
	_, err := svc.Reverse(context.Background(), "NOPE", domain.ReverseBlue)
	if !errors.Is(err, ErrVoucherNotFound) {
		t.Fatalf("voucher not found should give ErrVoucherNotFound, got %v", err)
	}
}

// TestReverseEntries_FlipsDCFlag verifies that reverseEntries produces entries
// with flipped DC flags and identical amounts — the core of red reversal
// immutability (a new voucher with opposite entries, original untouched).
func TestReverseEntries_FlipsDCFlag(t *testing.T) {
	origs := []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(5000), SubjectCode: "2011"},
		{TxnID: "T2", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(5000), SubjectCode: "2011"},
	}
	es := reverseEntries(origs)
	if len(es) != 2 {
		t.Fatalf("should be 2 reverse entries, got %d", len(es))
	}
	// Debit → Credit, Credit → Debit; amounts unchanged.
	if es[0].AccountNo != "D1" || es[0].DCFlag != domain.DCCredit {
		t.Errorf("D1 should flip to credit: %+v", es[0])
	}
	if es[1].AccountNo != "D2" || es[1].DCFlag != domain.DCDebit {
		t.Errorf("D2 should flip to debit: %+v", es[1])
	}
	if es[0].Amount != origs[0].Amount || es[1].Amount != origs[1].Amount {
		t.Errorf("reverse entry amount should be unchanged")
	}
}

func TestRecord_BizDateFromSysParam(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)
	booking, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("deposit should succeed: %v", err)
	}
	// biz_date is taken from fake store's GetBizDate (returns 2026-07-13), not time.Now()
	if booking.BizDate != "2026-07-13" {
		t.Errorf("biz_date should come from sys_param, got %q want 2026-07-13", booking.BizDate)
	}
}
