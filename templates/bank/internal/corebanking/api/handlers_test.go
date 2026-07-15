package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
)

type fakeAccounts struct {
	demand    *domain.DemandAccount
	fixed     *domain.FixedAccount
	demandErr error
}

func (f fakeAccounts) GetDemand(_ context.Context, _ string) (domain.DemandAccount, error) {
	if f.demandErr != nil {
		return domain.DemandAccount{}, f.demandErr
	}
	if f.demand != nil {
		return *f.demand, nil
	}
	return domain.DemandAccount{}, sql.ErrNoRows
}
func (f fakeAccounts) GetFixed(_ context.Context, _ string) (domain.FixedAccount, error) {
	if f.fixed != nil {
		return *f.fixed, nil
	}
	return domain.FixedAccount{}, sql.ErrNoRows
}

type fakeLedger struct{ gls []domain.GLBalance }

func (f fakeLedger) GetGL(context.Context, string) ([]domain.GLBalance, error) { return f.gls, nil }

type fakeTxnStore struct{ bal *domain.Balance }

func (f fakeTxnStore) ListTxns(context.Context, string, string, string) ([]domain.Txn, error) {
	return nil, nil
}
func (f fakeTxnStore) GetLatestBalance(context.Context, string) (domain.Balance, error) {
	if f.bal != nil {
		return *f.bal, nil
	}
	return domain.Balance{}, sql.ErrNoRows
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := getBody(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetAccount_Demand(t *testing.T) {
	h := &Handlers{Accounts: fakeAccounts{demand: &domain.DemandAccount{
		AccountNo: "D1", CustID: "C1", Ccy: "CNY", Status: domain.AccountStatusActive,
	}}}
	code, body := getBody(t, NewRouter(h), "/api/v1/accounts/D1")
	if code != 200 || !strings.Contains(body, `"cust_id":"C1"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_NotFound(t *testing.T) {
	h := &Handlers{Accounts: fakeAccounts{}}
	code, _ := getBody(t, NewRouter(h), "/api/v1/accounts/NOPE")
	if code != 404 {
		t.Errorf("want 404, got %d", code)
	}
}

func TestGetBalance(t *testing.T) {
	// 只读路径：写依赖 db/accounts/ledger/store 用 nil（Record 不触发），仅注入 read
	h := &Handlers{TxnSvc: service.NewTxnService(nil, nil, nil, nil).WithReader(fakeTxnStore{bal: &domain.Balance{
		AccountNo: "D1", BizDate: "2026-07-15", Balance: domain.NewMoneyFromCents(123456),
		AvailableBalance: domain.NewMoneyFromCents(123456),
	}})}
	code, body := getBody(t, NewRouter(h), "/api/v1/accounts/D1/balance")
	if code != 200 || !strings.Contains(body, `"balance":"1234.56"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetLedger_MissingBizDate(t *testing.T) {
	h := &Handlers{Ledger: fakeLedger{}}
	code, _ := getBody(t, NewRouter(h), "/api/v1/ledger")
	if code != 400 {
		t.Errorf("缺少 biz_date 应 400, got %d", code)
	}
}
