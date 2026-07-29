package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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

func assertStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != want {
		t.Fatalf("GET %s status=%d, want %d", path, recorder.Code, want)
	}
}
