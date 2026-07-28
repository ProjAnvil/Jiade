// Package grpcx contains the shared internal gRPC client and server setup.
package grpcx

import (
	"context"
	"time"

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
// and round-robin balancing. The caller's context is checked before creating
// the client because grpc.NewClient itself does not accept a context.
func Dial(ctx context.Context, cfg ClientConfig) (*grpc.ClientConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(defaultServiceConfig),
	}
	if cfg.Timeout > 0 {
		opts = append(opts, grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.DefaultConfig,
			MinConnectTimeout: cfg.Timeout,
		}))
	}
	return grpc.NewClient(cfg.Target, opts...)
}
