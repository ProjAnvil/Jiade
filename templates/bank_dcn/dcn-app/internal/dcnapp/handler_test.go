package dcnapp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"

	"bank-dcn/lib/mq"
	"bank-dcn/lib/sqltest"
)

// jsonEncode writes v as a JSON response body (test-double GNS/RMB endpoints).
func jsonEncode(w http.ResponseWriter, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// affResult is a driver.Result whose RowsAffected is fixed at construction.
type affResult int64

func (a affResult) LastInsertId() (int64, error) { return 0, nil }
func (a affResult) RowsAffected() (int64, error) { return int64(a), nil }

// pubCall records one publish invocation.
type pubCall struct {
	exchange, key, body string
}

func collectingPublish(pubs *[]pubCall) func(string, string, []byte) error {
	return func(exchange, key string, body []byte) error {
		*pubs = append(*pubs, pubCall{exchange, key, string(body)})
		return nil
	}
}

// NewServer wires config fields and defaults publishFn to the mq connection's Publish.
func TestNewServer(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	s := NewServer("dcn01", db, "http://gns", "http://rmb", &mq.Conn{}, 1000,
		decimal.RequireFromString("0.0001"))
	if s.dcn != "dcn01" || s.gns != "http://gns" || s.rmb != "http://rmb" ||
		s.rps != 1000 || s.hc == nil || s.publishFn == nil {
		t.Fatalf("NewServer fields not wired: %+v", s)
	}
}

// Handler mounts every business route plus /metrics behind the rate limiter.
func TestHandlerRoutes(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT balance FROM account WHERE account_id = ?",
			Columns: []string{"balance"}, Rows: [][]any{{"150.00"}}},
		sqltest.Rule{Contains: "COALESCE(SUM(balance), 0)",
			Columns: []string{"n", "s"}, Rows: [][]any{{int64(3), "450.00"}}},
	)
	s := newSrv("dcn01", db, "http://gns-unused", "http://rmb-unused")
	h := s.Handler()

	checks := []struct {
		method, path string
		code         int
		bodyHas      string
	}{
		{"GET", "/healthz", 200, "ok"},
		{"GET", "/accounts/1001/balance", 200, "150.00"},
		{"GET", "/accounts/abc/balance", 400, "invalid account id"},
		{"GET", "/internal/balance-sum", 200, `"accounts":3`},
		{"POST", "/transfer", 400, "fromId and toId required"},
		{"POST", "/internal/batch/interest", 400, "bizDate required"},
		{"GET", "/metrics", 200, ""},
	}
	for _, c := range checks {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader("{}")))
		if rec.Code != c.code {
			t.Fatalf("%s %s = %d (%s), want %d", c.method, c.path, rec.Code, rec.Body, c.code)
		}
		if c.bodyHas != "" && !strings.Contains(rec.Body.String(), c.bodyHas) {
			t.Fatalf("%s %s body %s missing %q", c.method, c.path, rec.Body, c.bodyHas)
		}
	}
}

// handleBalance: found / missing / query failure.
func TestHandleBalance(t *testing.T) {
	cases := []struct {
		name    string
		rules   []sqltest.Rule
		code    int
		bodyHas string
	}{
		{"found", []sqltest.Rule{
			{Contains: "SELECT balance FROM account WHERE account_id = ?",
				Columns: []string{"balance"}, Rows: [][]any{{"150.00"}}},
		}, 200, "150.00"},
		{"missing", []sqltest.Rule{
			{Contains: "SELECT balance FROM account WHERE account_id = ?",
				Columns: []string{"balance"}},
		}, 404, "account not found"},
		{"query error", []sqltest.Rule{
			{Contains: "SELECT balance FROM account WHERE account_id = ?", Err: errors.New("bal boom")},
		}, 500, "bal boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := sqltest.NewDB(t, tc.rules...)
			s := newSrv("dcn01", db, "http://gns", "http://rmb")
			r := httptest.NewRequest(http.MethodGet, "/accounts/1001/balance", nil)
			r.SetPathValue("id", "1001")
			w := httptest.NewRecorder()
			s.handleBalance(w, r)
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.bodyHas) {
				t.Fatalf("status = %d body = %s, want %d containing %q", w.Code, w.Body, tc.code, tc.bodyHas)
			}
		})
	}
}

// handleBalanceSum: renders the account count and balance sum; failures are 500.
func TestHandleBalanceSum(t *testing.T) {
	t.Run("aggregates", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "COALESCE(SUM(balance), 0)",
				Columns: []string{"n", "s"}, Rows: [][]any{{int64(3), "450.00"}}},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleBalanceSum(w, httptest.NewRequest(http.MethodGet, "/internal/balance-sum", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"balanceSum":"450.00"`) {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("query error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "COALESCE(SUM(balance), 0)", Err: errors.New("sum boom")},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleBalanceSum(w, httptest.NewRequest(http.MethodGet, "/internal/balance-sum", nil))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "sum boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
}

// handleCreateAccount: validation and persistence failures are rejected before any event.
func TestHandleCreateAccountValidation(t *testing.T) {
	cases := []struct{ name, body, wantErr string }{
		{"malformed body", "not json", "accountId and name required"},
		{"missing name", `{"accountId":1001}`, "accountId and name required"},
		{"non-numeric balance", `{"accountId":1001,"name":"a","initBalance":"abc"}`, "non-negative decimal"},
		{"negative balance", `{"accountId":1001,"name":"a","initBalance":"-1"}`, "non-negative decimal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := sqltest.NewDB(t)
			s := newSrv("dcn01", db, "http://gns", "http://rmb")
			w := httptest.NewRecorder()
			s.handleCreateAccount(w, httptest.NewRequest(http.MethodPost, "/accounts", strings.NewReader(tc.body)))
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Fatalf("status = %d body = %s, want 400 with %q", w.Code, w.Body, tc.wantErr)
			}
		})
	}
	t.Run("insert failure", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO account", Err: errors.New("insert boom")},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleCreateAccount(w, httptest.NewRequest(http.MethodPost, "/accounts",
			strings.NewReader(`{"accountId":1001,"name":"a","initBalance":"1.00"}`)))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "insert boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
}

// handleCreateAccount: success paths for zero vs positive initial balance, and tolerance
// of ADM event publish failures (at-most-once reporting is not request-critical).
func TestHandleCreateAccountCreated(t *testing.T) {
	t.Run("zero initial balance publishes no adm event", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: "INSERT INTO account"})
		var pubs []pubCall
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		s.publishFn = collectingPublish(&pubs)
		w := httptest.NewRecorder()
		s.handleCreateAccount(w, httptest.NewRequest(http.MethodPost, "/accounts",
			strings.NewReader(`{"accountId":1001,"name":"a","initBalance":"0"}`)))
		if w.Code != 201 || !strings.Contains(w.Body.String(), "created") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
		if len(pubs) != 0 {
			t.Fatalf("zero balance must not be reported, got %v", pubs)
		}
	})
	t.Run("positive initial balance is credited to adm", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: "INSERT INTO account"})
		var pubs []pubCall
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		s.publishFn = collectingPublish(&pubs)
		w := httptest.NewRecorder()
		s.handleCreateAccount(w, httptest.NewRequest(http.MethodPost, "/accounts",
			strings.NewReader(`{"accountId":1001,"name":"a","initBalance":"100.00"}`)))
		if w.Code != 201 {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
		if len(pubs) != 1 || pubs[0].exchange != "adm.events" ||
			!strings.Contains(pubs[0].body, "init-1001") || !strings.Contains(pubs[0].body, "CREDIT") {
			t.Fatalf("adm event = %+v", pubs)
		}
	})
	t.Run("adm publish failure is tolerated", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: "INSERT INTO account"})
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		s.publishFn = func(string, string, []byte) error { return errors.New("mq down") }
		w := httptest.NewRecorder()
		s.handleCreateAccount(w, httptest.NewRequest(http.MethodPost, "/accounts",
			strings.NewReader(`{"accountId":1001,"name":"a","initBalance":"100.00"}`)))
		if w.Code != 201 {
			t.Fatalf("status = %d body = %s, want 201 despite publish failure", w.Code, w.Body)
		}
	})
}

// mysql duplicate error helper shared by step/journal idempotency tests.
func dupErr() error { return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"} }
