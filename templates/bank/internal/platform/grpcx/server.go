package grpcx

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

const readinessTimeout = 5 * time.Second

var readinessTrackers sync.Map // map[*grpc.Server]*readinessTracker

// ServerConfig supplies the dependency readiness check for an internal server.
type ServerConfig struct {
	// Ready runs once in a lifecycle context owned by the returned server. It
	// MUST return promptly when ctx is canceled. Go cannot forcibly terminate a
	// callback that ignores cancellation; it remains tracked until it exits.
	Ready func(context.Context) error
}

// GracefulStopper is the portion of grpc.Server used for bounded shutdown.
type GracefulStopper interface {
	GracefulStop()
	Stop()
}

type readinessTracker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewServer creates a gRPC server with the standard health implementation. It
// remains NOT_SERVING until its tracked dependency readiness lifecycle succeeds.
func NewServer(cfg ServerConfig) *grpc.Server {
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	startReadiness(server, healthServer, cfg.Ready)
	return server
}

func startReadiness(server *grpc.Server, healthServer *health.Server, ready func(context.Context) error) {
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	tracker := &readinessTracker{cancel: cancel, done: make(chan struct{})}
	readinessTrackers.Store(server, tracker)
	go func() {
		defer close(tracker.done)
		defer readinessTrackers.Delete(server)
		readyCtx, timeoutCancel := context.WithTimeout(lifecycleCtx, readinessTimeout)
		defer timeoutCancel()
		if ready == nil || ready(readyCtx) == nil {
			healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
		}
	}()
}

func cancelReadiness(server *grpc.Server) {
	if value, ok := readinessTrackers.Load(server); ok {
		value.(*readinessTracker).cancel()
	}
}

func hasReadinessTracker(server *grpc.Server) bool {
	_, ok := readinessTrackers.Load(server)
	return ok
}

// Shutdown cancels readiness before draining RPCs. It waits for in-flight RPCs
// only until ctx expires, then forces the gRPC server to stop so process
// termination cannot hang indefinitely. grpc's GracefulStop may remain blocked
// after Stop, so the forced-stop path returns without waiting for it.
func Shutdown(ctx context.Context, server GracefulStopper) {
	if grpcServer, ok := server.(*grpc.Server); ok {
		cancelReadiness(grpcServer)
	}
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
