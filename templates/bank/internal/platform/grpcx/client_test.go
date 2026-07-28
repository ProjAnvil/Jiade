package grpcx

import (
	"context"
	"testing"
	"time"
)

func TestDefaultServiceConfigEnablesRoundRobinAndHealthChecking(t *testing.T) {
	const want = `{"loadBalancingConfig":[{"round_robin":{}}],"healthCheckConfig":{"serviceName":""}}`
	if defaultServiceConfig != want {
		t.Fatalf("default service config = %s, want %s", defaultServiceConfig, want)
	}
}

func TestDialIsLazySoTimeoutIsNotAnRPCCallDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := Dial(ctx, ClientConfig{Target: "dns:///customer:9090", Timeout: time.Hour})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
}
