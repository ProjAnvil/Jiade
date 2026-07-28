package grpcx

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	readinessTimeout       = 5 * time.Second
	readinessProbeInterval = time.Second
)

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
// NOT_SERVING until a health probe observes successful dependency readiness.
// Readiness always runs in a tracked health RPC, so a forced server Stop does
// not wait for a callback that ignores its cancellation context.
func NewServer(cfg ServerConfig) *grpc.Server {
	server := grpc.NewServer()
	baseHealth := health.NewServer()
	baseHealth.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpc_health_v1.RegisterHealthServer(server, &readinessHealthServer{base: baseHealth, ready: cfg.Ready})
	return server
}

type readinessHealthServer struct {
	grpc_health_v1.UnimplementedHealthServer
	base  *health.Server
	ready func(context.Context) error
}

func (s *readinessHealthServer) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	if req.GetService() == "" {
		s.probe(ctx)
	}
	return s.base.Check(ctx, req)
}

// Watch preserves the standard health-stream protocol for the overall service
// while re-probing dependencies until their status changes. Named services are
// delegated to the standard implementation unchanged.
func (s *readinessHealthServer) Watch(req *grpc_health_v1.HealthCheckRequest, stream grpc_health_v1.Health_WatchServer) error {
	if req.GetService() != "" {
		return s.base.Watch(req, stream)
	}

	ticker := time.NewTicker(readinessProbeInterval)
	defer ticker.Stop()
	var last grpc_health_v1.HealthCheckResponse_ServingStatus
	haveLast := false
	for {
		s.probe(stream.Context())
		response, err := s.base.Check(stream.Context(), req)
		if err != nil {
			return err
		}
		if !haveLast || response.Status != last {
			if err := stream.Send(response); err != nil {
				return err
			}
			last, haveLast = response.Status, true
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
		}
	}
}

func (s *readinessHealthServer) probe(ctx context.Context) {
	if s.ready == nil {
		s.base.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		return
	}
	readyCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	if s.ready(readyCtx) == nil {
		s.base.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		return
	}
	s.base.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
}

// Shutdown waits for in-flight RPCs only until ctx expires, then forces the
// gRPC server to stop so process termination cannot hang indefinitely. grpc's
// GracefulStop may remain blocked after Stop, so the forced-stop path returns
// without waiting for the graceful goroutine.
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
	}
}
