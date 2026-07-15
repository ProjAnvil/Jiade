package api

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
	"bank/internal/platform/pg"
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

// --- B-3 write-path tests ---

func postJSON(t *testing.T, h http.Handler, path, body string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// newRecordSvc 构造一个用 recordingAPIStore fake 的真 TxnService（db=nil 单测路径）。
func newRecordSvc() *service.TxnService {
	store := &recordingAPIStore{}
	return service.NewTxnService(nil, apiAccountsRdr{m: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, service.NewLedgerService(store), store)
}

// apiAccountsRdr 记账用的账户只读 fake（实现 service.AccountReader）。
type apiAccountsRdr struct{ m map[string]domain.DemandAccount }

func (a apiAccountsRdr) GetDemand(_ context.Context, no string) (domain.DemandAccount, error) {
	if v, ok := a.m[no]; ok {
		return v, nil
	}
	return domain.DemandAccount{}, sql.ErrNoRows
}

// recordingAPIStore 最小 LedgerStore fake：InsertTxns 回填假 ID，EnsureBalanceRow 给大余额（防透支）。
type recordingAPIStore struct{}

func (recordingAPIStore) InsertTxns(_ context.Context, _ pg.DBTX, txns []domain.Txn) error {
	for i := range txns {
		txns[i].TxnID = "T-api"
	}
	return nil
}
func (recordingAPIStore) ApplyBalanceDeltas(context.Context, pg.DBTX, string, []domain.BalanceDelta) error {
	return nil
}
func (recordingAPIStore) UpsertGL(context.Context, pg.DBTX, domain.GLBalance) error { return nil }
func (recordingAPIStore) EnsureBalanceRow(_ context.Context, _ pg.DBTX, _, _, _ string) (domain.Balance, error) {
	return domain.Balance{AvailableBalance: domain.NewMoneyFromCents(999999)}, nil
}
func (recordingAPIStore) GetTxnsByVoucher(context.Context, pg.DBTX, string) ([]domain.Txn, error) {
	return nil, nil
}
func (recordingAPIStore) UpdateTxnStatus(context.Context, pg.DBTX, string, domain.TxnStatus) error {
	return nil
}
func (recordingAPIStore) SetTxnSummary(context.Context, pg.DBTX, string, string) error { return nil }

func TestPostTxn_Deposit_201(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, body := postJSON(t, NewRouter(h), "/api/v1/txns",
		`{"action":"deposit","account_no":"D1","amount":"100.00","ccy":"CNY"}`)
	if code != 201 || !strings.Contains(body, `"voucher_no"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestPostTxn_BadRequest_MissingAction(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, _ := postJSON(t, NewRouter(h), "/api/v1/txns", `{"account_no":"D1","amount":"1.00"}`)
	if code != 400 {
		t.Errorf("缺 action 应 400, got %d", code)
	}
}

func TestPostTxn_AccountNotFound_404(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, _ := postJSON(t, NewRouter(h), "/api/v1/txns",
		`{"action":"deposit","account_no":"NOPE","amount":"1.00","ccy":"CNY"}`)
	if code != 404 {
		t.Errorf("账户不存在应 404, got %d", code)
	}
}
