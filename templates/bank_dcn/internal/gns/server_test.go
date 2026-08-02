package gns

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"bank_dcn/internal/platform/sqltest"
)

func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: time.Millisecond,
		ReadTimeout: time.Millisecond, WriteTimeout: time.Millisecond, MaxRetries: -1,
	})
}

func openReq(body string) *http.Request {
	return httptest.NewRequest("POST", "/accounts", strings.NewReader(body))
}

// requestId hit: directly returns the original account opening result without allocating a new ID or calling the DCN.
func TestOpenAccountIdempotentByRequestID(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"},
			Rows:    [][]any{{int64(1001), "dcn01", "http://dcn01-app:8080"}}},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"alice","initBalance":"100.00","requestId":"r-1"}`))
	if recorder.Code != 200 {
		t.Fatalf("status = %d (body: %s)", recorder.Code, recorder.Body)
	}
	var v LocateResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &v); err != nil || v.AccountID != 1001 {
		t.Fatalf("resp = %s", recorder.Body)
	}
	if rec.ExecCount("INSERT INTO account_route") != 0 {
		t.Fatal("idempotent hit must not allocate")
	}
}

// DCN account creation failure: rolls back the route row and returns 502, keeping routes and entities consistent.
func TestOpenAccountRollsBackRouteOnDCNFailure(t *testing.T) {
	dcn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dcn.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"}}, // empty set → no match
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows:    [][]any{{"dcn01", int64(1000), int64(1999), dcn.URL, "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}}, // zero counts
		sqltest.Rule{Contains: "MAX(account_id)",
			Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}}, // empty segment
		sqltest.Rule{Contains: "INSERT INTO account_route"},
		sqltest.Rule{Contains: "DELETE FROM account_route"},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"bob","initBalance":"100.00","requestId":"r-2"}`))
	if recorder.Code != 502 {
		t.Fatalf("status = %d, want 502 (body: %s)", recorder.Code, recorder.Body)
	}
	if rec.ExecCount("DELETE FROM account_route") != 1 {
		t.Fatal("route row should be rolled back")
	}
	del := rec.LastExec("DELETE FROM account_route")
	if del.Args[0] != int64(1001) {
		t.Fatalf("rollback args = %v, want account_id 1001", del.Args)
	}
}
