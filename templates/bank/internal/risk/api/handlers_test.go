package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/risk/domain"
	"bank/internal/risk/service"
)

type fakeRiskRepo struct {
	detail *domain.RiskEventDetail
	rules  []domain.RiskRule
}

func (f fakeRiskRepo) ListEvents(context.Context, string, string, string, string, int, int) ([]domain.RiskEvent, error) {
	return nil, nil
}
func (f fakeRiskRepo) GetEvent(context.Context, string) (domain.RiskEventDetail, error) {
	if f.detail != nil {
		return *f.detail, nil
	}
	return domain.RiskEventDetail{}, sql.ErrNoRows
}
func (f fakeRiskRepo) ListRules(context.Context) ([]domain.RiskRule, error) { return f.rules, nil }
func (f fakeRiskRepo) ListBlacklists(context.Context, string, int, int) ([]domain.Blacklist, error) {
	return nil, nil
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

func TestGetEvent_OK(t *testing.T) {
	d := &domain.RiskEventDetail{}
	d.EventID = "E1"
	d.CustID = "C1"
	d.RiskScore = "0.73"
	d.ActionTaken = "拦截"
	d.CustName = "张伟"
	h := &Handlers{Svc: service.NewRiskService(fakeRiskRepo{detail: d})}
	code, body := get(t, NewRouter(h), "/api/v1/risk/events/E1")
	if code != 200 || !strings.Contains(body, `"cust_name":"张伟"`) || !strings.Contains(body, `"risk_score":"0.73"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetEvent_NotFound(t *testing.T) {
	h := &Handlers{Svc: service.NewRiskService(fakeRiskRepo{})}
	code, _ := get(t, NewRouter(h), "/api/v1/risk/events/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}
