package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMountExposesMetricsAndCountsRequests(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /accounts/{id}/balance", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := Mount("testsvc", inner)

	// 业务请求被计数
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/1001/balance", nil))
	if rec.Code != 200 {
		t.Fatalf("inner handler code = %d", rec.Code)
	}

	// /metrics 暴露且包含本服务计数；handler 标签用路由模板而非原始路径（防基数爆炸）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `http_requests_total{code="200",handler="/accounts/{id}/balance",service="testsvc"} 1`) {
		t.Fatalf("metrics missing request count:\n%s", body)
	}
	if !strings.Contains(body, `http_request_duration_seconds_count{code_class="2xx",handler="/accounts/{id}/balance",service="testsvc"} 1`) {
		t.Fatalf("metrics missing duration count:\n%s", body)
	}
}

func TestIncTx(t *testing.T) {
	IncTx("TESTSTATUS")
	rec := httptest.NewRecorder()
	Mount("x", http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `rmb_tx_total{status="TESTSTATUS"} 1`) {
		t.Fatalf("missing rmb_tx_total:\n%s", rec.Body.String())
	}
}
