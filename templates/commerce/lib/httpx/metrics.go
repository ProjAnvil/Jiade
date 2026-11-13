package httpx

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newHTTPMetrics(registry prometheus.Registerer) *httpMetrics {
	requests := registerCounterVec(registry, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of Commerce HTTP requests.",
	}, []string{"service", "method", "code"}))
	duration := registerHistogramVec(registry, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Duration of Commerce HTTP requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"service", "method"}))
	return &httpMetrics{requests: requests, duration: duration}
}

func registerCounterVec(registry prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registry.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			panic(err)
		}
		return existing
	}
	return collector
}

func registerHistogramVec(registry prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := registry.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			panic(err)
		}
		return existing
	}
	return collector
}

func (metrics *httpMetrics) observe(service, method string, status int, elapsed time.Duration) {
	if metrics == nil {
		return
	}
	method = boundedHTTPMethod(method)
	code := "other"
	if status >= 100 && status <= 599 {
		code = strconv.Itoa(status)
	}
	metrics.requests.WithLabelValues(service, method, code).Inc()
	metrics.duration.WithLabelValues(service, method).Observe(elapsed.Seconds())
}

func boundedHTTPMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions,
		http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
