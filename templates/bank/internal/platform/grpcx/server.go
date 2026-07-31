package grpcx

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
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
	server := grpc.NewServer(
		// otelgrpc's stats handler propagates the W3C trace context through
		// gRPC metadata and records standard RPC spans on the server side.
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		// A unary interceptor renames the stats handler's server span to the
		// brief-mandated name (bank.grpc.<domain>.<Method>); the stats
		// handler's span is already in the context by the time the unary
		// interceptor runs on the server side.
		grpc.UnaryInterceptor(bankUnaryServerInterceptor),
	)
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

// bankUnaryServerInterceptor renames the otelgrpc stats handler's server span
// to the brief-mandated name (bank.grpc.<domain>.<Method>). On the server side
// the stats handler runs before the interceptor chain, so its span is already
// in the context; SetName mutates that same span in place before it ends.
func bankUnaryServerInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	trace.SpanFromContext(ctx).SetName(bankGRPCSpanName(info.FullMethod))
	return handler(ctx, req)
}
