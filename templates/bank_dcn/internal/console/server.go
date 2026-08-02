// Package console implements the observability console service: an embedded
// single-page app (topology view + status wall + RPS curve), backed by the
// Prometheus HTTP API and the Docker Engine API (read-only).
package console

import (
	"context"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"bank_dcn/internal/platform/metrics"
)

//go:embed index.html
var indexHTML []byte

// Server is the observability console service.
type Server struct {
	prom   string
	hc     *http.Client
	docker *http.Client
}

// NewServer builds the console; dockerSocket is the Docker Engine unix socket path.
func NewServer(promURL, dockerSocket string) *Server {
	return &Server{
		prom: promURL,
		hc:   &http.Client{Timeout: 5 * time.Second},
		docker: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", dockerSocket)
				},
			},
		},
	}
}

// Handler returns the router; business routes are registered via metrics.Handle to carry RED metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	metrics.Handle(mux, "console", "GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	}))
	metrics.Handle(mux, "console", "GET /api/targets", s.proxyProm("/api/v1/targets?state=active"))
	metrics.Handle(mux, "console", "GET /api/query", http.HandlerFunc(s.handleQuery))
	metrics.Handle(mux, "console", "GET /api/containers", http.HandlerFunc(s.handleContainers))
	return mux
}

// proxyProm proxies a fixed-path Prometheus API.
func (s *Server) proxyProm(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, s.prom+path)
	}
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, s.prom+"/api/v1/query?query="+url.QueryEscape(r.URL.Query().Get("query")))
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	resp, err := s.docker.Get("http://docker/containers/json?all=1")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":"docker unreachable"}`))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) proxy(w http.ResponseWriter, target string) {
	resp, err := s.hc.Get(target)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = w.Write([]byte(`{"error":"upstream unreachable"}`))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
