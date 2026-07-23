package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"commerce/internal/platform/telemetry"
)

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

func TestHandlerDiscardsWrittenResponseWhenHandlerPanics(t *testing.T) {
	s := NewServer(ServerConfig{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		_, _ = w.Write([]byte("partial response"))
		panic("unexpected")
	})})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if got := w.Body.String(); got == "partial response" || bytes.Contains(w.Body.Bytes(), []byte("partial response")) {
		t.Fatalf("body = %q, want no partial response", got)
	}
	if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", got)
	}
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
