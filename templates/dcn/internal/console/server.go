// Package console 实现观测台服务：内嵌单页（拓扑视图 + 状态墙 + RPS 曲线），
// 数据源为 Prometheus HTTP API 与 Docker Engine API（只读）。
package console

import (
	"context"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"dcn/internal/platform/metrics"
)

//go:embed index.html
var indexHTML []byte

// Server 是观测台服务。
type Server struct {
	prom   string
	hc     *http.Client
	docker *http.Client
}

// NewServer 构造观测台；dockerSocket 为 Docker Engine unix socket 路径。
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

// Handler 返回路由；业务路由经 metrics.Handle 注册以带 RED 指标。
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

// proxyProm 代理固定路径的 Prometheus API。
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
		http.Error(w, `{"error":"docker unreachable"}`, 502)
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
		http.Error(w, `{"error":"upstream unreachable"}`, 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
