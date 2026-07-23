package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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

func assertStatus(t *testing.T, h http.Handler, path string, want int) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != want {
		t.Fatalf("GET %s status = %d, want %d", path, w.Code, want)
	}
}
