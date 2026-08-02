// Package metrics provides unified Prometheus instrumentation: RED metrics
// middleware, a /metrics endpoint, and RMB transaction counting.
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

// Wrap collects the three RED signals. The handler label is supplied by the
// caller as the route template (e.g. /accounts/{id}/balance) to avoid
// exploding cardinality with raw paths.
// Note: go 1.22's ServeMux does not expose r.Pattern (added in go 1.23), so
// the label can only be passed explicitly at registration time.
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

// Handle registers pattern on mux with RED collection; the handler label is
// the route template (method prefix stripped).
func Handle(mux *http.ServeMux, service, pattern string, h http.Handler) {
	label := pattern
	if i := strings.IndexByte(label, ' '); i >= 0 {
		label = label[i+1:]
	}
	mux.Handle(pattern, Wrap(service, label, h))
}

// Mount returns the top-level handler with a /metrics endpoint; /metrics
// itself is not counted.
// Business routes must be registered via Handle to get RED metrics.
func Mount(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", h)
	return mux
}

// IncTx records one RMB global transaction reaching a final status
// (status: COMMITTED/COMPENSATED/FAILED).
func IncTx(status string) { txTotal.WithLabelValues(status).Inc() }
