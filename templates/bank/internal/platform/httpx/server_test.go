package httpx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestLiveAndReadyUseDifferentDependencySemantics(t *testing.T) {
	var ready atomic.Bool
	server := NewServer(ServerConfig{
		Service:  "payment",
		Instance: "payment-1",
		Ready: func(context.Context) error {
			if !ready.Load() {
				return errors.New("database unavailable")
			}
			return nil
		},
	})

	assertStatus(t, server.server.Handler, "/livez", http.StatusOK)
	assertStatus(t, server.server.Handler, "/readyz", http.StatusServiceUnavailable)
	ready.Store(true)
	assertStatus(t, server.server.Handler, "/readyz", http.StatusOK)
}

func TestShutdownDrainsReadinessWithoutStoppingLiveness(t *testing.T) {
	server := NewServer(ServerConfig{})

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertStatus(t, server.server.Handler, "/livez", http.StatusOK)
	assertStatus(t, server.server.Handler, "/readyz", http.StatusServiceUnavailable)
}

func TestServerExposesMetricsAndApplicationHandler(t *testing.T) {
	registry := prometheus.NewRegistry()
	server := NewServer(ServerConfig{
		Registry: registry,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}),
	})

	assertStatus(t, server.server.Handler, "/metrics", http.StatusOK)
	assertStatus(t, server.server.Handler, "/payments", http.StatusCreated)
}

func TestActiveServerDrainsInFlightRequestWhileLivezStaysHealthy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := NewServer(ServerConfig{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	listener := &singleConnListener{conn: serverConn, closed: make(chan struct{})}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.server.Serve(listener) }()

	request, err := http.NewRequest(http.MethodGet, "http://bank/work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Write(clientConn); err != nil {
		t.Fatal(err)
	}
	<-started

	shutdownResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdownResult <- server.Shutdown(ctx)
	}()
	waitForStatus(t, server.server.Handler, "/readyz", http.StatusServiceUnavailable)
	assertStatus(t, server.server.Handler, "/livez", http.StatusOK)
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned before in-flight request completed: %v", err)
	default:
	}

	close(release)
	response, err := http.ReadResponse(bufio.NewReader(clientConn), request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("response status=%d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if err := <-shutdownResult; err != nil {
		t.Fatal(err)
	}
	if err := <-serveResult; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve error=%v, want http.ErrServerClosed", err)
	}
}

func assertStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != want {
		t.Fatalf("GET %s status=%d, want %d", path, recorder.Code, want)
	}
}

func waitForStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s status=%d, want %d", path, recorder.Code, want)
		}
		time.Sleep(time.Millisecond)
	}
}

type singleConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func (listener *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	listener.once.Do(func() { conn = listener.conn })
	if conn != nil {
		return conn, nil
	}
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *singleConnListener) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return nil
}

func (listener *singleConnListener) Addr() net.Addr {
	return pipeAddr("bank-http")
}

type pipeAddr string

func (address pipeAddr) Network() string { return "pipe" }
func (address pipeAddr) String() string  { return string(address) }
