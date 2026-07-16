package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/reward/domain"
	"bank/internal/reward/service"
)

type fakeRewardRepo struct {
	acct    *domain.PointsAcct
	profile *domain.RewardProfile
	coupons []domain.Coupon
}

func (f fakeRewardRepo) GetPointsAcct(context.Context, string) (domain.PointsAcct, error) {
	if f.acct != nil {
		return *f.acct, nil
	}
	return domain.PointsAcct{}, sql.ErrNoRows
}
func (f fakeRewardRepo) ListPointsAccts(context.Context, string, int, int) ([]domain.PointsAcct, error) {
	return nil, nil
}
func (f fakeRewardRepo) ListCoupons(context.Context, string, string, int, int) ([]domain.Coupon, error) {
	return f.coupons, nil
}
func (f fakeRewardRepo) GetProfile(context.Context, string) (domain.RewardProfile, error) {
	if f.profile != nil {
		return *f.profile, nil
	}
	return domain.RewardProfile{}, sql.ErrNoRows
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
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetPointsAcct_OK(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{acct: &domain.PointsAcct{
		CustID: "C1", PointsBalance: 800, MemberLevel: "L3",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/reward/points-accounts/C1")
	if code != 200 || !strings.Contains(body, `"points_balance":800`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetPointsAcct_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/reward/points-accounts/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}

func TestGetProfile(t *testing.T) {
	h := &Handlers{Svc: service.NewRewardService(fakeRewardRepo{profile: &domain.RewardProfile{
		CustID: "C1", PointsBalance: 800, MemberLevel: "L3", CustName: "张伟", CustType: "个人",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/reward/customers/C1/profile")
	if code != 200 || !strings.Contains(body, `"cust_name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
