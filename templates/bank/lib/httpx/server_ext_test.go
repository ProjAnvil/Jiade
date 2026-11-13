package httpx

// Tests for ListenAndServe/IsClosed, the terminal lifecycle pair not covered
// by server_test.go (which drives requests against httptest servers).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestListenAndServeReturnsErrServerClosedAfterShutdown(t *testing.T) {
	// Bind an ephemeral port so we never collide with a real listener.
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	server := NewServer(ServerConfig{
		Service: "test-svc",
		Instance: "test-1",
		Addr:    net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Handler: http.NotFoundHandler(),
	})

	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()

	// Drain gracefully, then observe the documented terminal error.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if !IsClosed(err) {
			t.Fatalf("ListenAndServe terminal error = %v, want http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

func TestIsClosed(t *testing.T) {
	if IsClosed(nil) {
		t.Error("IsClosed(nil) must be false")
	}
	if IsClosed(errors.New("boom")) {
		t.Error("IsClosed(other) must be false")
	}
	if !IsClosed(http.ErrServerClosed) {
		t.Error("IsClosed(http.ErrServerClosed) must be true")
	}
}
