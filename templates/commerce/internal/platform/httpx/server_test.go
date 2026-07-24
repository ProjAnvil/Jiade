package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"commerce/internal/platform/telemetry"
)

func TestServerUsesConfiguredHTTPTimeouts(t *testing.T) {
	config := ServerConfig{
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	server := NewServer(config)
	if server.server.ReadHeaderTimeout != config.ReadHeaderTimeout ||
		server.server.ReadTimeout != config.ReadTimeout ||
		server.server.WriteTimeout != config.WriteTimeout ||
		server.server.IdleTimeout != config.IdleTimeout {
		t.Fatalf("timeouts read_header=%v read=%v write=%v idle=%v",
			server.server.ReadHeaderTimeout, server.server.ReadTimeout,
			server.server.WriteTimeout, server.server.IdleTimeout)
	}
}

func TestLiveAndReadyHaveDifferentDependencySemantics(t *testing.T) {
	ready := atomic.Bool{}
	s := NewServer(ServerConfig{Service: "order", Instance: "order-1",
		Ready: func(context.Context) error {
			if ready.Load() {
				return nil
			}
			return errors.New("database unavailable")
		}})
	assertStatus(t, s.Handler(), "/livez", http.StatusOK)
	assertStatus(t, s.Handler(), "/readyz", http.StatusServiceUnavailable)
	ready.Store(true)
	assertStatus(t, s.Handler(), "/readyz", http.StatusOK)
}

func TestHandlerAddsRequestAndServiceInstanceHeaders(t *testing.T) {
	s := NewServer(ServerConfig{Service: "order", Instance: "order-1"})
	r := httptest.NewRequest(http.MethodGet, "/livez", nil)
	r.Header.Set("X-Request-ID", "request-7")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if got := w.Header().Get("X-Request-ID"); got != "request-7" {
		t.Fatalf("X-Request-ID = %q, want request-7", got)
	}
	if got := w.Header().Get("X-Service-Instance"); got != "order-1" {
		t.Fatalf("X-Service-Instance = %q, want order-1", got)
	}
}

func TestHandlerRecoversPanicsAsProblemJSON(t *testing.T) {
	s := NewServer(ServerConfig{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("unexpected")
	})})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestHandlerRepanicsAfterCommittedResponsePanics(t *testing.T) {
	s := NewServer(ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte("partial response"))
		panic("unexpected")
	})})
	w := httptest.NewRecorder()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", got)
		}
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
		}
		if got := w.Body.String(); got != "partial response" {
			t.Fatalf("body = %q, want partial response", got)
		}
	}()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestResponseRecorderPreservesDirectOptionalInterfaces(t *testing.T) {
	underlying := &capabilityRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &responseRecorder{ResponseWriter: underlying, status: http.StatusOK}

	flusher, ok := any(w).(http.Flusher)
	if !ok {
		t.Fatal("response recorder does not implement http.Flusher")
	}
	flusher.Flush()
	if !underlying.flushed || !w.Committed() || w.status != http.StatusOK {
		t.Fatalf("Flush() flushed=%v committed=%v status=%d", underlying.flushed, w.Committed(), w.status)
	}

	pusher, ok := any(w).(http.Pusher)
	if !ok {
		t.Fatal("response recorder does not implement http.Pusher")
	}
	if err := pusher.Push("/asset.js", nil); err != nil || underlying.pushed != "/asset.js" {
		t.Fatalf("Push() err=%v target=%q", err, underlying.pushed)
	}

	readerFrom, ok := any(w).(io.ReaderFrom)
	if !ok {
		t.Fatal("response recorder does not implement io.ReaderFrom")
	}
	if n, err := readerFrom.ReadFrom(strings.NewReader("payload")); err != nil || n != int64(len("payload")) {
		t.Fatalf("ReadFrom() n=%d err=%v", n, err)
	}
	if got := underlying.Body.String(); got != "payload" || w.bytes != len("payload") || !underlying.readFrom {
		t.Fatalf("body=%q bytes=%d delegated=%v", got, w.bytes, underlying.readFrom)
	}

	hijacker, ok := any(w).(http.Hijacker)
	if !ok {
		t.Fatal("response recorder does not implement http.Hijacker")
	}
	conn, _, err := hijacker.Hijack()
	if err != nil || !underlying.hijacked {
		t.Fatalf("Hijack() err=%v hijacked=%v", err, underlying.hijacked)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

func TestResponseControllerFlushesThroughServerResponseWrapper(t *testing.T) {
	flushed := false
	s := NewServer(ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("Flush() error = %v", err)
		}
	})})
	w := flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: &flushed}
	s.Handler().ServeHTTP(&w, httptest.NewRequest(http.MethodGet, "/", nil))
	if !flushed {
		t.Fatal("ResponseController.Flush() did not reach the underlying ResponseWriter")
	}
}

func TestFlushMarksResponseCommittedBeforePanic(t *testing.T) {
	s := NewServer(ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Fatalf("Flush() error = %v", err)
		}
		panic("unexpected")
	})})
	w := flushRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: new(bool)}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %v, want http.ErrAbortHandler", got)
		}
	}()
	s.Handler().ServeHTTP(&w, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAccessLogIsJSONAndIncludesServiceIdentity(t *testing.T) {
	var logs bytes.Buffer
	s := NewServer(ServerConfig{
		Service: "order",
		Logger:  telemetry.NewJSONLogger(&logs),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	})
	s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("access log is not JSON: %v; log=%q", err, logs.String())
	}
	if got := entry["service"]; got != "order" {
		t.Fatalf("service = %v, want order", got)
	}
	if got := entry["msg"]; got != "http request" {
		t.Fatalf("msg = %v, want http request", got)
	}
}

func assertStatus(t *testing.T, h http.Handler, path string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != want {
		t.Fatalf("GET %s status = %d, want %d", path, w.Code, want)
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed *bool
}

func (w *flushRecorder) Flush() {
	*w.flushed = true
	w.ResponseRecorder.Flush()
}

type capabilityRecorder struct {
	*httptest.ResponseRecorder
	flushed  bool
	hijacked bool
	pushed   string
	readFrom bool
}

func (w *capabilityRecorder) Flush() {
	w.flushed = true
	w.ResponseRecorder.Flush()
}

func (w *capabilityRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

func (w *capabilityRecorder) Push(target string, _ *http.PushOptions) error {
	w.pushed = target
	return nil
}

func (w *capabilityRecorder) ReadFrom(source io.Reader) (int64, error) {
	w.readFrom = true
	return io.Copy(w.ResponseRecorder, source)
}
