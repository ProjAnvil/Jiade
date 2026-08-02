package gns

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"dcn/internal/platform/sqltest"
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

// requestId 命中：直接返回首次开户结果，不再分配新账号、不调 DCN。
func TestOpenAccountIdempotentByRequestID(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"},
			Rows:    [][]any{{int64(1001), "dcn01", "http://dcn01-app:8080"}}},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"张三","initBalance":"100.00","requestId":"r-1"}`))
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

// DCN 建户失败：回滚路由行并返回 502，保证路由与实体一致。
func TestOpenAccountRollsBackRouteOnDCNFailure(t *testing.T) {
	dcn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dcn.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"}}, // 空集 → 未命中
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows:    [][]any{{"dcn01", int64(1000), int64(1999), dcn.URL, "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}}, // 空计数
		sqltest.Rule{Contains: "MAX(account_id)",
			Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}}, // 空号段
		sqltest.Rule{Contains: "INSERT INTO account_route"},
		sqltest.Rule{Contains: "DELETE FROM account_route"},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"李四","initBalance":"100.00","requestId":"r-2"}`))
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
