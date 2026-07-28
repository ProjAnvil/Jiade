package grpcx

import (
	"context"
	"testing"
)

func TestDefaultServiceConfigEnablesRoundRobinAndHealthChecking(t *testing.T) {
	const want = `{"loadBalancingConfig":[{"round_robin":{}}],"healthCheckConfig":{"serviceName":""}}`
	if defaultServiceConfig != want {
		t.Fatalf("default service config = %s, want %s", defaultServiceConfig, want)
	}
}

func TestDialAcceptsDNSRoundRobinTarget(t *testing.T) {
	conn, err := Dial(context.Background(), ClientConfig{Target: "dns:///customer:9090"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
}
