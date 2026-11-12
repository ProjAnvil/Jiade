package adm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank-dcn/lib/contracts"
	"bank-dcn/lib/sqltest"
)

// gnsServer serves a fixed /routes payload, mimicking the GNS route table.
func gnsServer(t *testing.T, routes []routeView) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(routes)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dcnServer reports a fixed balance-sum payload for /internal/balance-sum.
func dcnServer(t *testing.T, accounts int, balanceSum string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": accounts, "balanceSum": balanceSum,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadURL returns the URL of an already-closed server (connection refused).
func deadURL() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

// Handler routing: every registered route reaches its handler; unmatched
// methods and paths fall through to the mux defaults (405/404).
func TestHandlerRouting(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "GROUP BY dcn",
			Columns: []string{"dcn", "COUNT(*)", "COALESCE(SUM(balance), 0)"},
			Rows:    [][]any{{"dcn01", int64(2), "100.00"}}},
		sqltest.Rule{Contains: "FROM global_balance",
			Columns: []string{"COUNT(*)", "COALESCE(SUM(balance), 0)"},
			Rows:    [][]any{{int64(2), "100.00"}}},
	)
	h := NewServer(db, "http://127.0.0.1:1").Handler() // gns unused except by /reconcile

	cases := []struct {
		name, method, target string
		wantCode             int
		wantSub              string
	}{
		{"healthz", "GET", "/healthz", 200, `"status":"ok"`},
		{"summary", "GET", "/report/summary", 200, "dcn01"},
		{"reconcile gns unreachable", "GET", "/reconcile", 502, "gns unreachable"},
		{"method not allowed", "POST", "/report/summary", 405, ""},
		{"unknown path", "GET", "/unknown", 404, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantSub != "" && !strings.Contains(rec.Body.String(), c.wantSub) {
				t.Fatalf("body %s should contain %q", rec.Body, c.wantSub)
			}
		})
	}
}

// handleSummary: totals plus a per-DCN breakdown built from the mirror table.
func TestHandleSummary(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "GROUP BY dcn",
			Columns: []string{"dcn", "COUNT(*)", "COALESCE(SUM(balance), 0)"},
			Rows: [][]any{
				{"dcn01", int64(2), "100.00"},
				{"dcn02", int64(1), "50.00"},
			}},
		sqltest.Rule{Contains: "FROM global_balance",
			Columns: []string{"COUNT(*)", "COALESCE(SUM(balance), 0)"},
			Rows:    [][]any{{int64(3), "150.00"}}},
	)
	s := NewServer(db, "http://gns-unused")
	rec := httptest.NewRecorder()
	s.handleSummary(rec, httptest.NewRequest("GET", "/report/summary", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body)
	}
	var v struct {
		Accounts     int    `json:"accounts"`
		TotalBalance string `json:"totalBalance"`
		PerDcn       []struct {
			DCN      string `json:"dcn"`
			Accounts int    `json:"accounts"`
			Total    string `json:"totalBalance"`
		} `json:"perDcn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Accounts != 3 || v.TotalBalance != "150.00" || len(v.PerDcn) != 2 ||
		v.PerDcn[0].DCN != "dcn01" || v.PerDcn[0].Accounts != 2 || v.PerDcn[0].Total != "100.00" {
		t.Fatalf("summary = %+v", v)
	}
}

// handleSummary: aggregate query failure, per-DCN query failure and scan
// failure all return 500.
func TestHandleSummaryErrors(t *testing.T) {
	t.Run("aggregate query fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FROM global_balance", Err: fmt.Errorf("db down")})
		rec := httptest.NewRecorder()
		NewServer(db, "http://gns-unused").handleSummary(rec,
			httptest.NewRequest("GET", "/report/summary", nil))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("per-dcn query fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			// Max:1 so only the first (aggregate) query matches this rule
			sqltest.Rule{Contains: "FROM global_balance", Max: 1,
				Columns: []string{"COUNT(*)", "COALESCE(SUM(balance), 0)"},
				Rows:    [][]any{{int64(0), "0"}}},
			sqltest.Rule{Contains: "GROUP BY dcn", Err: fmt.Errorf("db down")},
		)
		rec := httptest.NewRecorder()
		NewServer(db, "http://gns-unused").handleSummary(rec,
			httptest.NewRequest("GET", "/report/summary", nil))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("per-dcn scan fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FROM global_balance", Max: 1,
				Columns: []string{"COUNT(*)", "COALESCE(SUM(balance), 0)"},
				Rows:    [][]any{{int64(0), "0"}}},
			// one column cannot fill the three scan targets
			sqltest.Rule{Contains: "GROUP BY dcn",
				Columns: []string{"dcn"}, Rows: [][]any{{"dcn01"}}},
		)
		rec := httptest.NewRecorder()
		NewServer(db, "http://gns-unused").handleSummary(rec,
			httptest.NewRequest("GET", "/report/summary", nil))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// handleReconcile: mismatching totals are reported as inconsistent with both
// totals and the per-DCN breakdown in the response.
func TestReconcileInconsistentTotals(t *testing.T) {
	dcn := dcnServer(t, 2, "200.00")
	gns := gnsServer(t, []routeView{{DCN: "dcn01", Endpoint: dcn.URL, Status: "ACTIVE"}})
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SUM(balance)",
			Columns: []string{"COALESCE(SUM(balance), 0)"}, Rows: [][]any{{"100.00"}}},
	)
	s := NewServer(db, gns.URL)
	rec := httptest.NewRecorder()
	s.handleReconcile(rec, httptest.NewRequest("GET", "/reconcile", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body)
	}
	var v struct {
		Consistent bool     `json:"consistent"`
		AdmTotal   string   `json:"admTotal"`
		DcnTotal   string   `json:"dcnTotal"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// decimal.String() strips trailing zeros: "100.00" → "100"
	if v.Consistent || v.AdmTotal != "100" || v.DcnTotal != "200" || len(v.Errors) != 0 {
		t.Fatalf("reconcile = %+v", v)
	}
	if !strings.Contains(rec.Body.String(), "dcn01") {
		t.Fatal("perDcn should include dcn01")
	}
}

// handleReconcile: non-ACTIVE units are never polled; with no ACTIVE units the
// DCN total is zero.
func TestReconcileSkipsNonActive(t *testing.T) {
	gns := gnsServer(t, []routeView{{DCN: "dcn03", Endpoint: deadURL(), Status: "DRAINING"}})
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SUM(balance)",
			Columns: []string{"COALESCE(SUM(balance), 0)"}, Rows: [][]any{{"0"}}},
	)
	s := NewServer(db, gns.URL)
	rec := httptest.NewRecorder()
	s.handleReconcile(rec, httptest.NewRequest("GET", "/reconcile", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body)
	}
	var v struct {
		Consistent bool             `json:"consistent"`
		DcnTotal   string           `json:"dcnTotal"`
		PerDcn     []map[string]any `json:"perDcn"`
		Errors     []string         `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Consistent || v.DcnTotal != "0" || len(v.PerDcn) != 0 || len(v.Errors) != 0 {
		t.Fatalf("reconcile = %+v (DRAINING unit must not be polled)", v)
	}
}

// handleReconcile failure modes: GNS transport/decode failures → 502; per-DCN
// failures are collected in errors instead of failing the whole report; a
// mirror-table failure → 500.
func TestReconcileFailureModes(t *testing.T) {
	sumRule := sqltest.Rule{Contains: "SUM(balance)",
		Columns: []string{"COALESCE(SUM(balance), 0)"}, Rows: [][]any{{"100.00"}}}

	t.Run("gns unreachable", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sumRule)
		rec := httptest.NewRecorder()
		NewServer(db, deadURL()).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "gns unreachable") {
			t.Fatalf("resp = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("gns returns invalid json", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer bad.Close()
		db, _ := sqltest.NewDB(t, sumRule)
		rec := httptest.NewRecorder()
		NewServer(db, bad.URL).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "invalid gns response") {
			t.Fatalf("resp = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("dcn unreachable", func(t *testing.T) {
		gns := gnsServer(t, []routeView{{DCN: "dcn01", Endpoint: deadURL(), Status: "ACTIVE"}})
		db, _ := sqltest.NewDB(t, sumRule)
		rec := httptest.NewRecorder()
		NewServer(db, gns.URL).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "dcn01") ||
			strings.Contains(rec.Body.String(), `"consistent":true`) {
			t.Fatalf("resp = %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("dcn returns invalid json", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("oops"))
		}))
		defer bad.Close()
		gns := gnsServer(t, []routeView{{DCN: "dcn01", Endpoint: bad.URL, Status: "ACTIVE"}})
		db, _ := sqltest.NewDB(t, sumRule)
		rec := httptest.NewRecorder()
		NewServer(db, gns.URL).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if !strings.Contains(rec.Body.String(), "invalid response") {
			t.Fatalf("resp = %s", rec.Body)
		}
	})

	t.Run("dcn reports bad balanceSum", func(t *testing.T) {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": 1, "balanceSum": "abc"})
		}))
		defer bad.Close()
		gns := gnsServer(t, []routeView{{DCN: "dcn01", Endpoint: bad.URL, Status: "ACTIVE"}})
		db, _ := sqltest.NewDB(t, sumRule)
		rec := httptest.NewRecorder()
		NewServer(db, gns.URL).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if !strings.Contains(rec.Body.String(), "bad balanceSum") {
			t.Fatalf("resp = %s", rec.Body)
		}
	})

	t.Run("mirror query fails", func(t *testing.T) {
		gns := gnsServer(t, nil) // no ACTIVE units: fail at the mirror query
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SUM(balance)", Err: fmt.Errorf("db down")})
		rec := httptest.NewRecorder()
		NewServer(db, gns.URL).handleReconcile(rec,
			httptest.NewRequest("GET", "/reconcile", nil))
		if rec.Code != 500 {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// handleEvent: malformed JSON and non-numeric amounts are dropped silently
// (returning nil) without touching the database.
func TestHandleEventSkipsMalformed(t *testing.T) {
	db, rec := sqltest.NewDB(t) // no rules: any statement would surface as an error
	s := NewServer(db, "http://gns-unused")

	if err := s.handleEvent([]byte(`{"txId":`)); err != nil {
		t.Fatalf("malformed json should be ignored, got %v", err)
	}
	bad, _ := json.Marshal(contracts.BalanceEvent{
		TxID: "tx-bad", AccountID: 1001, DCN: "dcn01",
		Direction: "CREDIT", Amount: "abc",
	})
	if err := s.handleEvent(bad); err != nil {
		t.Fatalf("bad amount should be ignored, got %v", err)
	}
	if rec.ExecCount("") != 0 {
		t.Fatal("no statement should run for malformed events")
	}
}

// handleEvent: DEBIT events flip the sign of the mirror delta.
func TestHandleEventDebitNegatesDelta(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO event_log"},
		sqltest.Rule{Contains: "INSERT INTO global_balance"},
	)
	s := NewServer(db, "http://gns-unused")
	body, _ := json.Marshal(contracts.BalanceEvent{
		TxID: "tx-debit", AccountID: 1002, DCN: "dcn01",
		Direction: "DEBIT", Amount: "10.00",
	})
	if err := s.handleEvent(body); err != nil {
		t.Fatal(err)
	}
	ins := rec.LastExec("INSERT INTO global_balance")
	if len(ins.Args) != 3 || ins.Args[2] != "-10" {
		t.Fatalf("global_balance args = %v, want delta -10", ins.Args)
	}
}

// handleEvent: statement failures (other than duplicate keys) propagate so the
// broker can nack and requeue.
func TestHandleEventInsertFailures(t *testing.T) {
	t.Run("event_log fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO event_log", Err: fmt.Errorf("db down")})
		s := NewServer(db, "http://gns-unused")
		body, _ := json.Marshal(contracts.BalanceEvent{
			TxID: "tx-1", AccountID: 1001, DCN: "dcn01",
			Direction: "CREDIT", Amount: "1.00",
		})
		if err := s.handleEvent(body); err == nil {
			t.Fatal("event_log failure must propagate")
		}
	})
	t.Run("global_balance fails", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO event_log"},
			sqltest.Rule{Contains: "INSERT INTO global_balance", Err: fmt.Errorf("db down")},
		)
		s := NewServer(db, "http://gns-unused")
		body, _ := json.Marshal(contracts.BalanceEvent{
			TxID: "tx-2", AccountID: 1001, DCN: "dcn01",
			Direction: "CREDIT", Amount: "1.00",
		})
		if err := s.handleEvent(body); err == nil {
			t.Fatal("global_balance failure must propagate")
		}
	})
}
