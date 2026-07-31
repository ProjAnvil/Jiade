// Package grpcx contains the shared internal gRPC client and server setup.
package grpcx

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}],"healthCheckConfig":{"serviceName":""}}`

// ClientConfig describes an internal service target.
type ClientConfig struct {
	Target  string
	Timeout time.Duration
}

// Dial builds a lazy internal gRPC client using DNS resolution, health checks,
// and round-robin balancing. cfg.Timeout is grpc's MinConnectTimeout for each
// connection attempt, not a blocking dial timeout; callers set RPC deadlines
// on their request contexts. The caller's context is checked before creating
// the client because grpc.NewClient itself does not accept a context.
func Dial(ctx context.Context, cfg ClientConfig) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(defaultServiceConfig),
		// otelgrpc's stats handler propagates the W3C trace context through
		// gRPC metadata and records standard RPC spans.
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		// A unary interceptor stamps the brief-mandated span name
		// (bank.grpc.<domain>.<Method>) onto the client-side span; otelgrpc
		// v0.57.0 has no WithSpanNameFormatter for stats handlers.
		grpc.WithUnaryInterceptor(bankUnaryClientInterceptor),
	}
	if cfg.Timeout > 0 {
		opts = append(opts, grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.DefaultConfig,
			MinConnectTimeout: cfg.Timeout,
		}))
	}
	return grpc.NewClient(cfg.Target, opts...)
}

// grpcTracerName is the OpenTelemetry tracer name shared by the client and
// server interceptors for the bank's internal gRPC path.
const grpcTracerName = "bank.grpc"

// bankUnaryClientInterceptor records a client-side span carrying the
// brief-mandated name (bank.grpc.<domain>.<Method>). It passes its span context
// to the invoker so the stats handler's child span and the gRPC metadata
// propagation both descend from the brief-named span, keeping the trace
// continuous across the wire.
func bankUnaryClientInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx, span := otel.Tracer(grpcTracerName).Start(ctx, bankGRPCSpanName(method))
	defer span.End()
	return invoker(ctx, method, req, reply, cc, opts...)
}

// bankGRPCSpanName converts a gRPC full method name like
// "/bank.customer.v1.CustomerQueryService/GetCustomer" into the brief-mandated
// span name "bank.grpc.customer.GetCustomer". The second path segment of the
// proto package (the domain — customer, core, …) is used as the grouping key so
// every bank gRPC service maps to bank.grpc.<domain>.<Method>.
func bankGRPCSpanName(fullMethod string) string {
	trimmed := strings.TrimPrefix(fullMethod, "/")
	slash := strings.LastIndex(trimmed, "/")
	if slash < 0 {
		return fullMethod
	}
	servicePath := trimmed[:slash]
	method := trimmed[slash+1:]
	segments := strings.Split(servicePath, ".")
	if len(segments) >= 2 {
		return "bank.grpc." + segments[1] + "." + method
	}
	return fullMethod
}
