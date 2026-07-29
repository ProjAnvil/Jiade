// Package httpx provides the shared production HTTP lifecycle for bank services.
package httpx

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ServerConfig configures a bank service HTTP server.
type ServerConfig struct {
	Service         string
	Instance        string
	Addr            string
	Handler         http.Handler
	Ready           func(context.Context) error
	Registry        *prometheus.Registry
	ShutdownTimeout time.Duration
}

// Server owns an HTTP server and its drain state.
type Server struct {
	server          *http.Server
	ready           func(context.Context) error
	shuttingDown    atomic.Bool
	shutdownTimeout time.Duration
}

// NewServer creates an HTTP server with application, health, and metrics routes.
func NewServer(config ServerConfig) *Server {
	if config.Addr == "" {
		config.Addr = ":8080"
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 5 * time.Second
	}
	if config.Registry == nil {
		config.Registry = prometheus.NewRegistry()
	}

	server := &Server{
		ready:           config.Ready,
		shutdownTimeout: config.ShutdownTimeout,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", server.readiness)
	mux.Handle("/metrics", promhttp.HandlerFor(config.Registry, promhttp.HandlerOpts{}))
	if config.Handler != nil {
		mux.Handle("/", config.Handler)
	}
	server.server = &http.Server{
		Addr:              config.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
	}
	return server
}

// ListenAndServe starts serving at the configured address.
func (server *Server) ListenAndServe() error {
	return server.server.ListenAndServe()
}

// Shutdown marks the service unready before gracefully draining HTTP requests.
func (server *Server) Shutdown(ctx context.Context) error {
	server.shuttingDown.Store(true)
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, server.shutdownTimeout)
		defer cancel()
	}
	return server.server.Shutdown(ctx)
}

func (server *Server) readiness(w http.ResponseWriter, request *http.Request) {
	if server.shuttingDown.Load() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if server.ready != nil {
		if err := server.ready(request.Context()); err != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// IsClosed reports the normal terminal error returned by ListenAndServe after
// Shutdown.
func IsClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed)
}
