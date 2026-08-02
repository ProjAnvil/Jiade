package adm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"bank_dcn/internal/contracts"
	"bank_dcn/internal/platform/sqltest"
)

func eventBody(t *testing.T, txID string) []byte {
	t.Helper()
	b, _ := json.Marshal(contracts.BalanceEvent{
		TxID: txID, AccountID: 1001, DCN: "dcn01", Direction: "CREDIT", Amount: "10.00",
	})
	return b
}

// handleEvent: duplicate events are deduplicated by uk_event; global_balance is incremented only once.
func TestHandleEventIdempotent(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO event_log", Max: 1}, // first attempt succeeds
		sqltest.Rule{Contains: "INSERT INTO event_log",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}}, // duplicate
		sqltest.Rule{Contains: "INSERT INTO global_balance"},
	)
	s := NewServer(db, "http://gns-unused")
	if err := s.handleEvent(eventBody(t, "tx-dup")); err != nil {
		t.Fatal(err)
	}
	if err := s.handleEvent(eventBody(t, "tx-dup")); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("INSERT INTO global_balance") != 1 {
		t.Fatalf("mirror should be updated exactly once, got %d",
			rec.ExecCount("INSERT INTO global_balance"))
	}
}

// handleReconcile: ADM mirror total == sum of per-unit real-time totals → consistent=true.
func TestReconcileConsistent(t *testing.T) {
	unit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dcn": "dcn01", "accounts": 2, "balanceSum": "100.00",
		})
	}))
	defer unit.Close()
	gns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]routeView{{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"}})
	}))
	defer gns.Close()

	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM global_balance",
			Columns: []string{"COALESCE(SUM(balance), 0)"}, Rows: [][]any{{"100.00"}}},
	)
	s := NewServer(db, gns.URL)
	rec := httptest.NewRecorder()
	s.handleReconcile(rec, httptest.NewRequest("GET", "/reconcile", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var v struct {
		Consistent bool   `json:"consistent"`
		AdmTotal   string `json:"admTotal"`
		DcnTotal   string `json:"dcnTotal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// decimal.String() strips trailing zeros: "100.00" → "100"
	if !v.Consistent || v.AdmTotal != "100" || v.DcnTotal != "100" {
		t.Fatalf("reconcile = %+v", v)
	}
	if !strings.Contains(rec.Body.String(), "dcn01") {
		t.Fatal("perDcn should include dcn01")
	}
}
