package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/customer/domain"
	"bank/internal/customer/service"
)

type fakeCustRepo struct {
	c      *domain.Customer
	accts  []domain.CustAccount
	getErr error
}

func (f fakeCustRepo) GetCustomer(context.Context, string) (domain.Customer, error) {
	if f.getErr != nil {
		return domain.Customer{}, f.getErr
	}
	if f.c != nil {
		return *f.c, nil
	}
	return domain.Customer{}, sql.ErrNoRows
}
func (f fakeCustRepo) ListCustomers(context.Context, string, string, int, int) ([]domain.Customer, error) {
	return nil, nil
}
func (f fakeCustRepo) GetCustAccounts(context.Context, string) ([]domain.CustAccount, error) {
	return f.accts, nil
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
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
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetCustomer_OK(t *testing.T) {
	h := &Handlers{Svc: service.NewCustomerService(fakeCustRepo{c: &domain.Customer{CustID: "C0000001", Name: "张伟", CustType: domain.CustTypePersonal}})}
	code, body := get(t, NewRouter(h), "/api/v1/customers/C0000001")
	if code != 200 || !strings.Contains(body, `"name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetCustomer_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewCustomerService(fakeCustRepo{})} // Return ErrNoRows
	code, _ := get(t, NewRouter(h), "/api/v1/customers/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}

func TestGetCustAccounts(t *testing.T) {
	h := &Handlers{Svc: service.NewCustomerService(fakeCustRepo{accts: []domain.CustAccount{{AccountNo: "D1", Ccy: "CNY", Status: "active", Role: "主"}}})}
	code, body := get(t, NewRouter(h), "/api/v1/customers/C0000001/accounts")
	if code != 200 || !strings.Contains(body, `"account_no":"D1"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
