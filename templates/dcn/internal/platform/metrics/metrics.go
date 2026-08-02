// Package metrics 提供统一的 Prometheus 埋点：RED 指标中间件、/metrics 端点、RMB 事务计数。
package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	reqTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by service/handler/code.",
	}, []string{"service", "handler", "code"})
	reqDur = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by service/handler.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"service", "handler", "code_class"})
	txTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rmb_tx_total", Help: "RMB global transactions reaching a final status.",
	}, []string{"status"})
)

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// middleware 采集 RED 三要素。handler 标签取 ServeMux 路由后的 r.Pattern
// （如 /accounts/{id}/balance），避免按原始路径炸基数；必须在 next 之后读取。
func middleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)
		// r.Pattern 含方法前缀（如 "GET /accounts/{id}/balance"），去掉方法只留路由模板。
		handler := r.Pattern
		if i := strings.IndexByte(handler, ' '); i >= 0 {
			handler = handler[i+1:]
		}
		if handler == "" {
			handler = "unmatched"
		}
		code := strconv.Itoa(sw.code)
		reqTotal.WithLabelValues(service, handler, code).Inc()
		reqDur.WithLabelValues(service, handler, code[:1]+"xx").Observe(time.Since(start).Seconds())
	})
}

// Mount 返回带 /metrics 端点与 RED 采集的总 handler；/metrics 自身不计数。
func Mount(service string, h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", middleware(service, h))
	return mux
}

// IncTx 记录一笔到达终态的 RMB 总事务（status: COMMITTED/COMPENSATED/FAILED）。
func IncTx(status string) { txTotal.WithLabelValues(status).Inc() }
