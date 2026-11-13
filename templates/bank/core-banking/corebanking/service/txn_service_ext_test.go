package service

// Tests for the read-only delegation methods (WithReader, ListTxns,
// GetBalance) on TxnService.

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"bank/core-banking/corebanking/domain"
)

type stubTxnStore struct {
	txns     []domain.Txn
	bal      *domain.Balance
	err      error
	gotAcct  string
	gotFrom  string
	gotTo    string
}

func (s *stubTxnStore) ListTxns(_ context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	s.gotAcct, s.gotFrom, s.gotTo = accountNo, from, to
	if s.err != nil {
		return nil, s.err
	}
	return s.txns, nil
}

func (s *stubTxnStore) GetLatestBalance(_ context.Context, accountNo string) (domain.Balance, error) {
	s.gotAcct = accountNo
	if s.err != nil {
		return domain.Balance{}, s.err
	}
	if s.bal != nil {
		return *s.bal, nil
	}
	return domain.Balance{}, sql.ErrNoRows
}

func TestTxnService_ReaderRequired(t *testing.T) {
	svc := NewTxnService(nil, nil, nil, nil)
	if _, err := svc.ListTxns(context.Background(), "", "", ""); err == nil {
		t.Error("ListTxns without a reader must fail")
	}
	if _, err := svc.GetBalance(context.Background(), "D1"); err == nil {
		t.Error("GetBalance without a reader must fail")
	}
}

func TestTxnService_WithReaderDelegates(t *testing.T) {
	store := &stubTxnStore{
		txns: []domain.Txn{{TxnID: "T1"}},
		bal:  &domain.Balance{AccountNo: "D1", BizDate: "2026-07-31"},
	}
	svc := NewTxnService(nil, nil, nil, nil).WithReader(store)
	if got := svc.WithReader(store); got != svc {
		t.Error("WithReader must return the service for chaining")
	}
	ctx := context.Background()

	txns, err := svc.ListTxns(ctx, "D1", "2026-07-01", "2026-07-31")
	if err != nil || len(txns) != 1 {
		t.Fatalf("ListTxns: %+v %v", txns, err)
	}
	if store.gotAcct != "D1" || store.gotFrom != "2026-07-01" || store.gotTo != "2026-07-31" {
		t.Errorf("filters wrong: %+v", store)
	}

	bal, err := svc.GetBalance(ctx, "D1")
	if err != nil || bal.AccountNo != "D1" {
		t.Fatalf("GetBalance: %+v %v", bal, err)
	}
}

func TestTxnService_ReaderErrorsSurface(t *testing.T) {
	boom := errors.New("boom")
	svc := NewTxnService(nil, nil, nil, nil).WithReader(&stubTxnStore{err: boom})
	if _, err := svc.ListTxns(context.Background(), "", "", ""); !errors.Is(err, boom) {
		t.Errorf("ListTxns err = %v", err)
	}
	if _, err := svc.GetBalance(context.Background(), "D1"); !errors.Is(err, boom) {
		t.Errorf("GetBalance err = %v", err)
	}
}
