package grpcx

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

// ServerConfig supplies the dependency readiness check for an internal server.
type ServerConfig struct {
	Ready func(context.Context) error
}

// NewServer creates a gRPC server whose standard health endpoint remains
// NOT_SERVING until dependencies are ready.
func NewServer(cfg ServerConfig) *grpc.Server {
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)

	go func() {
		if cfg.Ready == nil || cfg.Ready(context.Background()) == nil {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		}
	}()

	return server
}
