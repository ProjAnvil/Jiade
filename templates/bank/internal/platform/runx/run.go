// Package runx coordinates HTTP and optional gRPC process lifecycles.
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

// Serve runs HTTP and optional gRPC until ctx is canceled or a listener exits.
// Unexpected listener errors and HTTP shutdown errors are returned together.
func Serve(
	ctx context.Context,
	httpServer HTTPServer,
	grpcService *GRPCService,
	shutdownTimeout time.Duration,
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

	serverCount := 1
	results := make(chan serveResult, 2)
	go func() {
		results <- serveResult{protocol: "HTTP", err: httpServer.ListenAndServe()}
	}()
	if grpcService != nil {
		serverCount++
		go func() {
			results <- serveResult{protocol: "gRPC", err: grpcService.Server.Serve(grpcService.Listener)}
		}()
	}

	var runErr error
	received := 0
	select {
	case <-ctx.Done():
	case result := <-results:
		received++
		runErr = classifyServeResult(result, false)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	if grpcService != nil {
		grpcx.Shutdown(shutdownCtx, grpcService.Server)
	}

	for received < serverCount {
		select {
		case result := <-results:
			received++
			runErr = errors.Join(runErr, classifyServeResult(result, true))
		case <-shutdownCtx.Done():
			received = serverCount
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
	}
	if result.err == nil {
		return fmt.Errorf("%s server stopped unexpectedly", result.protocol)
	}
	return fmt.Errorf("%s server failed: %w", result.protocol, result.err)
}
