// Package runx coordinates HTTP, optional gRPC, and optional background worker
// process lifecycles under a single cancellation context.
package runx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"bank/internal/platform/grpcx"
	"google.golang.org/grpc"
)

// Worker is a long-running background task (command consumer, outbox relay,
// etc.) that runs until ctx is cancelled or it returns an error. Serve treats
// an unexpected Worker error (before ctx cancellation) as a process failure:
// the context is cancelled so HTTP/gRPC/other workers shut down, and the error
// is joined into the returned error.
type Worker interface {
	Run(context.Context) error
}

// WorkerFunc adapts a function to the Worker interface.
type WorkerFunc func(context.Context) error

// Run calls f(ctx).
func (f WorkerFunc) Run(ctx context.Context) error { return f(ctx) }


// HTTPServer is the shared HTTP lifecycle used by Serve.
type HTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

// GRPCServer is the shared gRPC lifecycle used by Serve.
type GRPCServer interface {
	grpcx.GracefulStopper
	Serve(net.Listener) error
}

// GRPCService binds a gRPC server to its listener.
type GRPCService struct {
	Server   GRPCServer
	Listener net.Listener
}

type serveResult struct {
	protocol string
	err      error
}

// Serve runs HTTP, optional gRPC, and optional background workers until ctx is
// canceled or any one component exits. Unexpected server/worker errors and HTTP
// shutdown errors are returned together. When any component exits, the internal
// context is cancelled so all remaining components shut down; the function then
// waits for all goroutines to finish (up to shutdownTimeout for HTTP/gRPC
// drain) before returning.
func Serve(
	ctx context.Context,
	httpServer HTTPServer,
	grpcService *GRPCService,
	shutdownTimeout time.Duration,
	workers ...Worker,
) error {
	if httpServer == nil {
		return errors.New("HTTP server is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if shutdownTimeout <= 0 {
		shutdownTimeout = 5 * time.Second
	}
	if grpcService != nil && grpcService.Server == nil {
		return errors.New("gRPC server is required")
	}

	// Internal cancellation: cancelled when ANY component exits or the caller
	// cancels the parent context. This ensures a failed worker stops HTTP/gRPC.
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	totalServers := 1
	results := make(chan serveResult, 2+len(workers))
	go func() {
		results <- serveResult{protocol: "HTTP", err: httpServer.ListenAndServe()}
	}()
	if grpcService != nil {
		totalServers++
		go func() {
			results <- serveResult{protocol: "gRPC", err: grpcService.Server.Serve(grpcService.Listener)}
		}()
	}
	for i, w := range workers {
		name := fmt.Sprintf("worker[%d]", i)
		go func() {
			results <- serveResult{protocol: name, err: w.Run(serveCtx)}
		}()
	}

	totalComponents := totalServers + len(workers)
	var runErr error
	received := 0
	// Wait until the parent context is cancelled OR one component exits.
	select {
	case <-serveCtx.Done():
	case result := <-results:
		received++
		runErr = errors.Join(runErr, classifyServeResult(result, false))
		// Cancel the internal context so the other components stop.
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if grpcService != nil {
		grpcx.Shutdown(shutdownCtx, grpcService.Server)
	}

	for received < totalComponents {
		select {
		case result := <-results:
			received++
			runErr = errors.Join(runErr, classifyServeResult(result, true))
		case <-shutdownCtx.Done():
			received = totalComponents
		}
	}
	return errors.Join(runErr, shutdownErr)
}

func classifyServeResult(result serveResult, stopping bool) error {
	switch result.protocol {
	case "HTTP":
		if errors.Is(result.err, http.ErrServerClosed) {
			return nil
		}
	case "gRPC":
		if errors.Is(result.err, grpc.ErrServerStopped) || stopping && result.err == nil {
			return nil
		}
	default:
		// Background workers: a nil error or context.Canceled during shutdown
		// is a graceful exit, not a failure.
		if result.err == nil {
			if stopping {
				return nil
			}
			return fmt.Errorf("%s stopped unexpectedly", result.protocol)
		}
		if errors.Is(result.err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("%s failed: %w", result.protocol, result.err)
	}
	if result.err == nil {
		return fmt.Errorf("%s server stopped unexpectedly", result.protocol)
	}
	return fmt.Errorf("%s server failed: %w", result.protocol, result.err)
}
