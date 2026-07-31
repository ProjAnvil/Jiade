package runx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServeReturnsUnexpectedHTTPErrorAndShutsDown(t *testing.T) {
	serveErr := errors.New("HTTP listener failed")
	httpServer := newFakeHTTPServer(serveErr, nil)

	err := Serve(context.Background(), httpServer, nil, time.Second)
	if !errors.Is(err, serveErr) {
		t.Fatalf("Serve error=%v, want %v", err, serveErr)
	}
	if httpServer.shutdownCalls() != 1 {
		t.Fatalf("Shutdown calls=%d, want 1", httpServer.shutdownCalls())
	}
}

func TestServeJoinsListenerAndShutdownErrors(t *testing.T) {
	serveErr := errors.New("HTTP listener failed")
	shutdownErr := errors.New("HTTP drain failed")
	httpServer := newFakeHTTPServer(serveErr, shutdownErr)

	err := Serve(context.Background(), httpServer, nil, time.Second)
	if !errors.Is(err, serveErr) || !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error=%v, want listener and shutdown errors", err)
	}
}

func TestServeReturnsUnexpectedGRPCErrorAndStopsBothServers(t *testing.T) {
	grpcErr := errors.New("gRPC listener failed")
	httpServer := newFakeHTTPServer(http.ErrServerClosed, nil)
	grpcServer := &fakeGRPCServer{serveErr: grpcErr}

	err := Serve(context.Background(), httpServer, &GRPCService{Server: grpcServer, Listener: nil}, time.Second)
	if !errors.Is(err, grpcErr) {
		t.Fatalf("Serve error=%v, want %v", err, grpcErr)
	}
	if httpServer.shutdownCalls() != 1 || grpcServer.gracefulCalls != 1 {
		t.Fatalf("HTTP shutdown=%d gRPC graceful=%d, want 1/1", httpServer.shutdownCalls(), grpcServer.gracefulCalls)
	}
}

func TestServeTreatsHTTPServerClosedAsNormal(t *testing.T) {
	httpServer := newFakeHTTPServer(http.ErrServerClosed, nil)

	if err := Serve(context.Background(), httpServer, nil, time.Second); err != nil {
		t.Fatalf("Serve error=%v, want nil for http.ErrServerClosed", err)
	}
}

func TestServeCancellationReturnsShutdownFailure(t *testing.T) {
	shutdownErr := errors.New("HTTP drain failed")
	httpServer := newFakeHTTPServer(http.ErrServerClosed, shutdownErr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Serve(ctx, httpServer, nil, time.Second)
	if !errors.Is(err, shutdownErr) {
		t.Fatalf("Serve error=%v, want %v", err, shutdownErr)
	}
}

type fakeHTTPServer struct {
	serveErr    error
	shutdownErr error
	mu          sync.Mutex
	shutdowns   int
}

func newFakeHTTPServer(serveErr, shutdownErr error) *fakeHTTPServer {
	return &fakeHTTPServer{serveErr: serveErr, shutdownErr: shutdownErr}
}

func (server *fakeHTTPServer) ListenAndServe() error { return server.serveErr }

func (server *fakeHTTPServer) Shutdown(context.Context) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.shutdowns++
	return server.shutdownErr
}

func (server *fakeHTTPServer) shutdownCalls() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.shutdowns
}

type fakeGRPCServer struct {
	serveErr      error
	gracefulCalls int
	stopCalls     int
}

func (server *fakeGRPCServer) Serve(net.Listener) error { return server.serveErr }
func (server *fakeGRPCServer) GracefulStop()            { server.gracefulCalls++ }
func (server *fakeGRPCServer) Stop()                    { server.stopCalls++ }

// --- Worker lifecycle tests ---

func TestServeWorkerFailureCancelsHTTPAndGRPC(t *testing.T) {
	httpServer := newFakeHTTPServer(http.ErrServerClosed, nil)
	grpcServer := &fakeGRPCServer{}
	workerErr := errors.New("consumer broker closed")
	worker := WorkerFunc(func(ctx context.Context) error {
		<-ctx.Done() // block until cancelled by test setup
		return workerErr
	})

	// Use a cancellable parent ctx so we can control timing: cancel after the
	// worker has a chance to be started.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := Serve(ctx, httpServer, &GRPCService{Server: grpcServer, Listener: nil}, time.Second, worker)
	if !errors.Is(err, workerErr) {
		t.Fatalf("Serve error=%v, want workerErr %v", err, workerErr)
	}
	if httpServer.shutdownCalls() != 1 || grpcServer.gracefulCalls != 1 {
		t.Fatalf("HTTP shutdown=%d gRPC graceful=%d, want 1/1", httpServer.shutdownCalls(), grpcServer.gracefulCalls)
	}
}

func TestServeContextCancelStopsWorker(t *testing.T) {
	httpServer := newFakeHTTPServer(http.ErrServerClosed, nil)
	workerExited := make(chan struct{})
	worker := WorkerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		close(workerExited)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	if err := Serve(ctx, httpServer, nil, time.Second, worker); err != nil {
		t.Fatalf("Serve error=%v, want nil on graceful shutdown", err)
	}
	select {
	case <-workerExited:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after context cancellation")
	}
}
