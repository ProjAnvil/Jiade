package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/loan/domain"
	"bank/internal/loan/service"
)

type fakeLoanRepo struct {
	account  *domain.LoanAccount
	profile  *domain.LoanProfile
	products []domain.LoanProduct
	// Record the latest ListAccounts parameter
	gotProductCode string
	gotStatus      string
	gotOffset      int
	gotLimit       int
}

func (f *fakeLoanRepo) ListProducts(context.Context) ([]domain.LoanProduct, error) {
	return f.products, nil
}

func (f *fakeLoanRepo) ListAccounts(_ context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	f.gotProductCode, f.gotStatus, f.gotOffset, f.gotLimit = productCode, status, offset, limit
	if f.account != nil {
		return []domain.LoanAccount{*f.account}, nil
	}
	return nil, nil
}

func (f *fakeLoanRepo) GetAccount(_ context.Context, loanNo string) (domain.LoanAccount, error) {
	if f.account != nil && f.account.LoanNo == loanNo {
		return *f.account, nil
	}
	return domain.LoanAccount{}, sql.ErrNoRows
}

func (f *fakeLoanRepo) ListBalances(context.Context, string, string, string, int, int) ([]domain.LoanBalance, error) {
	return nil, nil
}

func (f *fakeLoanRepo) ListOverdue(context.Context, string, string, string, int, int) ([]domain.LoanOverdue, error) {
	return nil, nil
}

func (f *fakeLoanRepo) GetProfile(_ context.Context, loanNo string) (domain.LoanProfile, error) {
	if f.profile != nil && f.profile.LoanNo == loanNo {
		return *f.profile, nil
	}
	return domain.LoanProfile{}, sql.ErrNoRows
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + path)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_OK(t *testing.T) {
	fake := &fakeLoanRepo{account: &domain.LoanAccount{
		LoanNo: "LN0000001", CustID: "C0000001", Principal: domain.NewMoneyFromCents(1000000), Balance: domain.NewMoneyFromCents(900000), Rate: "0.043500",
	}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts/LN0000001")
	if code != 200 || !strings.Contains(body, `"principal":"10000.00"`) || !strings.Contains(body, `"balance":"9000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewLoanService(&fakeLoanRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/loan/accounts/LN9999999")
	if code != 404 {
		t.Errorf("code=%d want 404", code)
	}
}

func TestListAccounts_FiltersAndPagination(t *testing.T) {
	fake := &fakeLoanRepo{account: &domain.LoanAccount{LoanNo: "LN0000001", CustID: "C1"}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts?product_code=LP-CONS&status=disbursed&offset=10&limit=5")
	if code != 200 || !strings.Contains(body, `"accounts"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
	if fake.gotProductCode != "LP-CONS" || fake.gotStatus != "disbursed" || fake.gotOffset != 10 || fake.gotLimit != 5 {
		t.Errorf("参数透传错: %+v", fake)
	}
}

func TestGetProfile(t *testing.T) {
	fake := &fakeLoanRepo{profile: &domain.LoanProfile{
		LoanNo: "LN0000001", CustID: "C0000001", Principal: domain.NewMoneyFromCents(1000000), Balance: domain.NewMoneyFromCents(900000), Rate: "0.043500", CustName: "张伟", CustType: "个人",
	}}
	h := &Handlers{Svc: service.NewLoanService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/loan/accounts/LN0000001/profile")
	if code != 200 || !strings.Contains(body, "张伟") || !strings.Contains(body, `"cust_type":"个人"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
