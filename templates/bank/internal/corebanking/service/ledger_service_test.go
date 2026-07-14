package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
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
	err := svc.Post(context.Background(), entries, "2026-07-15", "CNY")
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post 不平应返回 ErrUnbalanced, got %v", err)
	}
	if store.calls != 0 {
		t.Errorf("不平时不应调用 store, 调用次数=%d", store.calls)
	}
}

func TestPost_Balanced_Persists(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	if err := svc.Post(context.Background(), entries, "2026-07-15", "CNY"); err != nil {
		t.Fatalf("Post 平账应成功: %v", err)
	}
	if len(store.txns) != 2 {
		t.Errorf("应写 2 笔流水, got %d", len(store.txns))
	}
	if len(store.deltas) != 2 {
		t.Errorf("应更新 2 个账户余额, got %d", len(store.deltas))
	}
	if store.gl == nil {
		t.Error("应更新总账")
	}
}

// recordingLedgerStore 记录调用，用于断言 Post 的副作用。
type recordingLedgerStore struct {
	calls  int
	txns   []domain.Txn
	deltas []domain.BalanceDelta
	gl     *domain.GLBalance
}

func (f *recordingLedgerStore) InsertTxns(_ context.Context, txns []domain.Txn) error {
	f.calls++
	f.txns = append(f.txns, txns...)
	return nil
}
func (f *recordingLedgerStore) ApplyBalanceDeltas(_ context.Context, _ string, deltas []domain.BalanceDelta) error {
	f.calls++
	f.deltas = append(f.deltas, deltas...)
	return nil
}
func (f *recordingLedgerStore) UpsertGL(_ context.Context, gl domain.GLBalance) error {
	f.calls++
	f.gl = &gl
	return nil
}
