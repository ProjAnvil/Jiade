package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

func TestValidateBalance_Balanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	debit, credit, err := ValidateBalance(entries)
	if err != nil {
		t.Fatalf("平衡应无错: %v", err)
	}
	if debit != 10000 || credit != 10000 {
		t.Errorf("debit=%d credit=%d, want 10000/10000", debit, credit)
	}
}

func TestValidateBalance_Unbalanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 9999},
	}
	_, _, err := ValidateBalance(entries)
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("不平应返回 ErrUnbalanced, got %v", err)
	}
}

func TestPost_Unbalanced_RefusesAndDoesNotTouchStore(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 100},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 99},
	}
	_, err := svc.Post(context.Background(), nil, entries, "2026-07-15", "CNY", "V1", "")
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post 不平应返回 ErrUnbalanced, got %v", err)
	}
	if store.calls != 0 {
		t.Errorf("不平时不应调用 store, 调用次数=%d", store.calls)
	}
}

func TestPost_Balanced_PersistsAndReturnsTxns(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	txns, err := svc.Post(context.Background(), nil, entries, "2026-07-15", "CNY", "V1", "")
	if err != nil {
		t.Fatalf("Post 平账应成功: %v", err)
	}
	if len(txns) != 2 || txns[0].VoucherNo != "V1" {
		t.Errorf("应返回 2 条带 voucherNo 的流水, got %+v", txns)
	}
	if len(store.txns) != 2 || len(store.deltas) != 2 || store.gl == nil {
		t.Errorf("store 副作用不对: txns=%d deltas=%d gl=%v", len(store.txns), len(store.deltas), store.gl)
	}
}

type recordingLedgerStore struct {
	calls       int
	txns        []domain.Txn
	deltas      []domain.BalanceDelta
	gl          *domain.GLBalance
	voucherTxns []domain.Txn // GetTxnsByVoucher / LockTxnsByVoucher Return
	statusLog   []string     // Logging UpdateTxnStatus calls
	hasReversal bool         // HasReversal return value (default false)

	summaryCalls       int
	lastSummaryVoucher string
	lastSummary        string
}

func (f *recordingLedgerStore) InsertTxns(_ context.Context, _ pg.DBTX, txns []domain.Txn) error {
	f.calls++
	// Simulate repo backfill TxnID
	for i := range txns {
		if txns[i].TxnID == "" {
			txns[i].TxnID = "T-fake"
		}
	}
	f.txns = append(f.txns, txns...)
	return nil
}
func (f *recordingLedgerStore) ApplyBalanceDeltas(_ context.Context, _ pg.DBTX, _ string, deltas []domain.BalanceDelta) error {
	f.calls++
	f.deltas = append(f.deltas, deltas...)
	return nil
}
func (f *recordingLedgerStore) UpsertGL(_ context.Context, _ pg.DBTX, gl domain.GLBalance) error {
	f.calls++
	f.gl = &gl
	return nil
}
func (f *recordingLedgerStore) EnsureBalanceRow(context.Context, pg.DBTX, string, string, string) (domain.Balance, error) {
	f.calls++
	return domain.Balance{}, nil
}
func (f *recordingLedgerStore) GetTxnsByVoucher(_ context.Context, _ pg.DBTX, _ string) ([]domain.Txn, error) {
	f.calls++
	return f.voucherTxns, nil
}
func (f *recordingLedgerStore) LockTxnsByVoucher(_ context.Context, _ pg.DBTX, _ string) ([]domain.Txn, error) {
	f.calls++
	return f.voucherTxns, nil
}
func (f *recordingLedgerStore) HasReversal(_ context.Context, _ pg.DBTX, _ string) (bool, error) {
	f.calls++
	return f.hasReversal, nil
}
func (f *recordingLedgerStore) UpdateTxnStatus(_ context.Context, _ pg.DBTX, _ string, st domain.TxnStatus) error {
	f.calls++
	f.statusLog = append(f.statusLog, string(st))
	return nil
}
func (f *recordingLedgerStore) SetTxnSummary(_ context.Context, _ pg.DBTX, voucherNo, summary string) error {
	f.calls++
	f.summaryCalls++
	f.lastSummaryVoucher = voucherNo
	f.lastSummary = summary
	return nil
}
func (f *recordingLedgerStore) GetBizDate(context.Context) (string, error) {
	f.calls++
	return "2026-07-13", nil
}
