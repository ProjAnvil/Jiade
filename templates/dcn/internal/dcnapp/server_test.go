package dcnapp

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"

	"dcn/internal/platform/sqltest"
)

func newSrv(dcnID string, db *sql.DB, gnsURL, rmbURL string) *Server {
	return &Server{
		dcn: dcnID, db: db, gns: gnsURL, rmb: rmbURL,
		rps: 1000, rate: decimal.RequireFromString("0.0001"), hc: newHTTPClient(),
	}
}

func locateReply(dcn, endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"accountId": 1, "dcn": dcn, "endpoint": endpoint,
		})
	}
}

// 本单元转账：GNS 双方同单元 → 本地事务，200 ok。
func TestTransferLocalSameUnit(t *testing.T) {
	gns := httptest.NewServer(locateReply("dcn01", "http://dcn01-app:8080"))
	defer gns.Close()
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance - ?"},
		sqltest.Rule{Contains: "balance + ?"},
	)
	s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":1002,"amount":"10.00"}`)))
	if w.Code != 200 {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 2 {
		t.Fatalf("want 2 journal entries, got %d", rec.ExecCount("INSERT INTO journal"))
	}
}

// 跨单元转账：提交 RMB 且 COMMITTED → 200 ok。
func TestTransferCrossUnitViaRMB(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
		dcn, ep := "dcn01", "http://dcn01-app:8080"
		if r.URL.Query().Get("accountId") == "2001" {
			dcn, ep = "dcn02", "http://dcn02-app:8080"
		}
		json.NewEncoder(w).Encode(map[string]any{"accountId": 1, "dcn": dcn, "endpoint": ep})
	})
	gns := httptest.NewServer(mux)
	defer gns.Close()
	rmb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"txId": "tx-9", "status": "COMMITTED"})
	}))
	defer rmb.Close()

	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn01", db, gns.URL, rmb.URL)
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":2001,"amount":"10.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "tx-9") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}

// 接入层转发：源账户在他单元 → 原样转发到其 endpoint。
func TestTransferForwardsToSourceUnit(t *testing.T) {
	dcn01 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","txId":"fwd-1"}`))
	}))
	defer dcn01.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
		ep := "http://dcn02-app:8080"
		dcn := "dcn02"
		if r.URL.Query().Get("accountId") == "1001" {
			dcn, ep = "dcn01", dcn01.URL
		}
		json.NewEncoder(w).Encode(map[string]any{"accountId": 1, "dcn": dcn, "endpoint": ep})
	})
	gns := httptest.NewServer(mux)
	defer gns.Close()

	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn02", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":2002,"amount":"10.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fwd-1") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}

// 重复开户：MySQL 1062 → 200 exists。
func TestCreateAccountDuplicateReturnsExists(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO account",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
	)
	s := newSrv("dcn01", db, "http://gns-unused", "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleCreateAccount(w, httptest.NewRequest("POST", "/accounts",
		strings.NewReader(`{"accountId":1001,"name":"王五","initBalance":"100.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "exists") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}
