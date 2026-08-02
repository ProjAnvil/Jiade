package batch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateBizDate(t *testing.T) {
	for _, ok := range []string{"2026-08-02", "2026-12-31"} {
		if !validateBizDate(ok) {
			t.Errorf("validateBizDate(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "2026-8-2", "2026-13-01", "2026-02-30", "today"} {
		if validateBizDate(bad) {
			t.Errorf("validateBizDate(%q) = true, want false", bad)
		}
	}
}

// runOnUnits 并发调用各单元结息端点并归集结果（单元失败不拖垮整体）。
func TestRunOnUnits(t *testing.T) {
	okUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dcn": "dcn01", "accounts": 50, "totalInterest": "5.00",
		})
	}))
	defer okUnit.Close()
	badUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer badUnit.Close()

	s := &Server{hc: okUnit.Client()}
	units := []route{
		{DCN: "dcn01", Endpoint: okUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn02", Endpoint: badUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn03", Endpoint: "http://127.0.0.1:1", Status: "ACTIVE"}, // 连接失败
	}
	results := s.runOnUnits(context.Background(), "2026-08-02", units)
	if len(results) != 3 {
		t.Fatalf("results len = %d", len(results))
	}
	if results[0].Err != nil || results[0].Accounts != 50 || results[0].Interest != "5.00" {
		t.Errorf("dcn01 result = %+v", results[0])
	}
	if results[1].Err == nil {
		t.Errorf("dcn02 should fail with HTTP 500")
	}
	if results[2].Err == nil {
		t.Errorf("dcn03 should fail with connection error")
	}
}
