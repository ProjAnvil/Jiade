package grpcx

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

var readinessTimeout = 5 * time.Second

// ServerConfig supplies the dependency readiness check for an internal server.
type ServerConfig struct {
	Ready func(context.Context) error
}

// GracefulStopper is the portion of grpc.Server used for bounded shutdown.
type GracefulStopper interface {
	GracefulStop()
	Stop()
}

// NewServer creates a gRPC server whose standard health endpoint remains
// NOT_SERVING until dependencies are ready.
func NewServer(cfg ServerConfig) *grpc.Server {
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)

	go func() {
		readyCtx, cancel := context.WithTimeout(context.Background(), readinessTimeout)
		defer cancel()
		if cfg.Ready == nil || cfg.Ready(readyCtx) == nil {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		}
	}()

	return server
}

// Shutdown waits for in-flight RPCs only until ctx expires, then forces the
// gRPC server to stop so process termination cannot hang indefinitely.
func Shutdown(ctx context.Context, server GracefulStopper) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		server.Stop()
		<-done
	}
}
