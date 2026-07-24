package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"commerce/internal/platform/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const defaultBodyLimit = 1 << 20

// ServerConfig configures a production HTTP server.
type ServerConfig struct {
	Service           string
	Instance          string
	Addr              string
	Handler           http.Handler
	Ready             func(context.Context) error
	Registry          *prometheus.Registry
	Logger            *slog.Logger
	ShutdownTimeout   time.Duration
	RequestBodyLimit  int64
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Server owns an HTTP server and its readiness state.
type Server struct {
	server          *http.Server
	handler         http.Handler
	ready           func(context.Context) error
	shuttingDown    atomic.Bool
	shutdownTimeout time.Duration
}

// NewServer creates a server with health, metrics, safety, and observability middleware.
func NewServer(config ServerConfig) *Server {
	if config.Addr == "" {
		config.Addr = ":8080"
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 20 * time.Second
	}
	if config.RequestBodyLimit <= 0 {
		config.RequestBodyLimit = defaultBodyLimit
	}
	if config.ReadHeaderTimeout <= 0 {
		config.ReadHeaderTimeout = 5 * time.Second
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = 15 * time.Second
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = 30 * time.Second
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = time.Minute
	}
	if config.Logger == nil {
		config.Logger = telemetry.NewJSONLogger(os.Stderr)
	}

	server := &Server{ready: config.Ready, shutdownTimeout: config.ShutdownTimeout}
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", server.readiness)
	if config.Registry == nil {
		mux.Handle("/metrics", promhttp.Handler())
	} else {
		mux.Handle("/metrics", promhttp.HandlerFor(config.Registry, promhttp.HandlerOpts{}))
	}
	if config.Handler != nil {
		mux.Handle("/", config.Handler)
	}

	server.handler = requestID(serviceInstance(config.Instance,
		accessLog(config.Logger, config.Service,
			limitBody(config.RequestBodyLimit, recoverPanic(config.Logger, config.Service, mux)))))
	server.server = &http.Server{
		Addr:              config.Addr,
		Handler:           server.handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		ReadTimeout:       config.ReadTimeout,
		WriteTimeout:      config.WriteTimeout,
		IdleTimeout:       config.IdleTimeout,
	}
	return server
}

// Handler returns the configured HTTP handler for in-process tests and embedding.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve serves HTTP requests from listener.
func (s *Server) Serve(listener net.Listener) error { return s.server.Serve(listener) }

// ListenAndServe starts serving at the configured address.
func (s *Server) ListenAndServe() error { return s.server.ListenAndServe() }

// Shutdown marks the server unready and gracefully completes in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shuttingDown.Store(true)
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.shutdownTimeout)
		defer cancel()
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		WriteProblem(w, Problem{Status: http.StatusServiceUnavailable, Code: "not_ready"})
		return
	}
	if s.ready != nil {
		if err := s.ready(r.Context()); err != nil {
			WriteProblem(w, Problem{Status: http.StatusServiceUnavailable, Code: "not_ready"})
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ErrServerClosed reports the normal terminal error returned after Shutdown.
var ErrServerClosed = http.ErrServerClosed

// IsClosed reports whether err is the normal server-closed condition.
func IsClosed(err error) bool { return errors.Is(err, http.ErrServerClosed) }
