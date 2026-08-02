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

// Wrap 采集 RED 三要素。handler 标签由调用方给定为路由模板
// （如 /accounts/{id}/balance），避免按原始路径炸基数。
// 注：go 1.22 的 ServeMux 不提供 r.Pattern（go 1.23 才有），故标签只能注册时显式传入。
func Wrap(service, handler string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)
		code := strconv.Itoa(sw.code)
		reqTotal.WithLabelValues(service, handler, code).Inc()
		reqDur.WithLabelValues(service, handler, code[:1]+"xx").Observe(time.Since(start).Seconds())
	})
}

// Handle 在 mux 上注册 pattern 并附 RED 采集；handler 标签取路由模板（去掉方法前缀）。
func Handle(mux *http.ServeMux, service, pattern string, h http.Handler) {
	label := pattern
	if i := strings.IndexByte(label, ' '); i >= 0 {
		label = label[i+1:]
	}
	mux.Handle(pattern, Wrap(service, label, h))
}

// Mount 返回带 /metrics 端点的总 handler；/metrics 自身不计数。
// 业务路由需经 Handle 注册才有 RED 指标。
func Mount(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", h)
	return mux
}

// IncTx 记录一笔到达终态的 RMB 总事务（status: COMMITTED/COMPENSATED/FAILED）。
func IncTx(status string) { txTotal.WithLabelValues(status).Inc() }
