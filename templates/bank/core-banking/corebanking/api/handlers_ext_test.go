package api

// Handler tests for ListTxns and the error branches not covered by
// handlers_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"bank/core-banking/corebanking/domain"
	"bank/core-banking/corebanking/service"
)

type stubTxnReadStore struct {
	txns []domain.Txn
	err  error
	bal  *domain.Balance
}

func (s *stubTxnReadStore) ListTxns(context.Context, string, string, string) ([]domain.Txn, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.txns, nil
}

func (s *stubTxnReadStore) GetLatestBalance(context.Context, string) (domain.Balance, error) {
	if s.err != nil {
		return domain.Balance{}, s.err
	}
	if s.bal != nil {
		return *s.bal, nil
	}
	return domain.Balance{}, sql.ErrNoRows
}

type failingLedger struct{ err error }

func (f failingLedger) GetGL(context.Context, string) ([]domain.GLBalance, error) {
	return nil, f.err
}

func TestListTxns_OK(t *testing.T) {
	h := &Handlers{
		TxnSvc: service.NewTxnService(nil, nil, nil, nil).WithReader(&stubTxnReadStore{txns: []domain.Txn{{
			TxnID: "T1", BizDate: "2026-07-31", AccountNo: "D1", DCFlag: domain.DCDebit,
			Amount: domain.NewMoneyFromCents(10000), Summary: "deposit",
		}}}),
	}
	code, body := getBody(t, NewRouter(h), "/api/v1/txns?account_no=D1&from=2026-07-01&to=2026-07-31")
	if code != 200 || !strings.Contains(body, `"txn_id":"T1"`) || !strings.Contains(body, `"amount":"100.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestListTxns_EmptyIsArray(t *testing.T) {
	h := &Handlers{TxnSvc: service.NewTxnService(nil, nil, nil, nil).WithReader(&stubTxnReadStore{})}
	code, body := getBody(t, NewRouter(h), "/api/v1/txns")
	if code != 200 || !strings.Contains(body, `"txns":[]`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestListTxns_StoreErrorIs500(t *testing.T) {
	h := &Handlers{TxnSvc: service.NewTxnService(nil, nil, nil, nil).WithReader(&stubTxnReadStore{err: errors.New("db down")})}
	code, body := getBody(t, NewRouter(h), "/api/v1/txns")
	if code != 500 || !strings.Contains(body, "db down") {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestListTxns_MissingReaderIs500(t *testing.T) {
	// A TxnService wired without a read store fails the read endpoints;
	// the handler must surface 500 rather than panic.
	h := &Handlers{TxnSvc: service.NewTxnService(nil, nil, nil, nil)}
	if code, _ := getBody(t, NewRouter(h), "/api/v1/txns"); code != 500 {
		t.Errorf("code=%d want 500", code)
	}
}

func TestGetBalance_StoreErrorIs500(t *testing.T) {
	h := &Handlers{TxnSvc: service.NewTxnService(nil, nil, nil, nil).WithReader(&stubTxnReadStore{err: errors.New("boom")})}
	if code, _ := getBody(t, NewRouter(h), "/api/v1/accounts/D1/balance"); code != 500 {
		t.Errorf("code=%d want 500", code)
	}
}

func TestGetLedger_OK(t *testing.T) {
	h := &Handlers{Ledger: fakeLedger{gls: []domain.GLBalance{{
		SubjectCode: "1001", BizDate: "2026-07-31",
		DCBalance: domain.NewMoneyFromCents(100000), CCBalance: domain.NewMoneyFromCents(100000), Ccy: "CNY",
	}}}}
	code, body := getBody(t, NewRouter(h), "/api/v1/ledger?biz_date=2026-07-31")
	if code != 200 || !strings.Contains(body, `"subject_code":"1001"`) || !strings.Contains(body, `"dc":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetLedger_StoreErrorIs500(t *testing.T) {
	h := &Handlers{Ledger: failingLedger{err: errors.New("boom")}}
	code, body := getBody(t, NewRouter(h), "/api/v1/ledger?biz_date=2026-07-31")
	if code != 500 || !strings.Contains(body, "boom") {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_StoreErrorIs500(t *testing.T) {
	h := &Handlers{Accounts: fakeAccounts{demandErr: errors.New("boom")}}
	if code, _ := getBody(t, NewRouter(h), "/api/v1/accounts/D1"); code != 500 {
		t.Errorf("code=%d want 500", code)
	}
}
