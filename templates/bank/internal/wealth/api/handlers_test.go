package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/wealth/domain"
	"bank/internal/wealth/service"
)

type fakeWealthRepo struct {
	holding *domain.WealthHolding
	profile *domain.WealthProfile
	// Record the latest ListHoldings parameter
	gotCustID string
	gotOffset int
	gotLimit  int
}

func (f *fakeWealthRepo) ListProducts(context.Context) ([]domain.WealthProduct, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListNav(context.Context, string, string, string) ([]domain.WealthNav, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListHoldings(_ context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	f.gotCustID, f.gotOffset, f.gotLimit = custID, offset, limit
	if f.holding != nil {
		return []domain.WealthHolding{*f.holding}, nil
	}
	return nil, nil
}

func (f *fakeWealthRepo) ListOrders(context.Context, string, string, string, string, int, int) ([]domain.WealthOrder, error) {
	return nil, nil
}

func (f *fakeWealthRepo) ListIncomes(context.Context, string, string, string, int, int) ([]domain.WealthIncome, error) {
	return nil, nil
}

func (f *fakeWealthRepo) GetHoldingProfile(_ context.Context, holdingID string) (domain.WealthProfile, error) {
	if f.profile != nil && f.profile.HoldingID == holdingID {
		return *f.profile, nil
	}
	return domain.WealthProfile{}, sql.ErrNoRows
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

func TestListHoldings_FiltersAndPagination(t *testing.T) {
	fake := &fakeWealthRepo{holding: &domain.WealthHolding{
		HoldingID: "WP-HD-0000001", CustID: "C0000001", Share: "1050.2500", Cost: domain.NewMoneyFromCents(100000), CurrentValue: domain.NewMoneyFromCents(100000),
	}}
	h := &Handlers{Svc: service.NewWealthService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/wealth/holdings?cust_id=C0000001&offset=5&limit=10")
	if code != 200 || !strings.Contains(body, `"cost":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
	if fake.gotCustID != "C0000001" || fake.gotOffset != 5 || fake.gotLimit != 10 {
		t.Errorf("参数透传错: %+v", fake)
	}
}

func TestGetHoldingProfile_OK(t *testing.T) {
	fake := &fakeWealthRepo{profile: &domain.WealthProfile{
		HoldingID: "WP-HD-0000001", CustID: "C0000001", ProductCode: "WP-FIX1", Share: "1050.2500",
		CurrentValue: domain.NewMoneyFromCents(100000), CustName: "张伟", CustType: "个人",
	}}
	h := &Handlers{Svc: service.NewWealthService(fake)}
	code, body := get(t, NewRouter(h), "/api/v1/wealth/holdings/WP-HD-0000001/profile")
	if code != 200 || !strings.Contains(body, "张伟") || !strings.Contains(body, `"current_value":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetHoldingProfile_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewWealthService(&fakeWealthRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/wealth/holdings/WP-HD-9999999/profile")
	if code != 404 {
		t.Errorf("code=%d want 404", code)
	}
}
