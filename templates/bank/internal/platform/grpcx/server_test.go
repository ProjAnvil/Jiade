package grpcx

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func TestNewServerCancelsReadinessAfterTimeout(t *testing.T) {
	originalTimeout := readinessTimeout
	readinessTimeout = 10 * time.Millisecond
	t.Cleanup(func() { readinessTimeout = originalTimeout })

	canceled := make(chan struct{})
	NewServer(ServerConfig{Ready: func(ctx context.Context) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}})

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("readiness callback was not canceled after its timeout")
	}
}

func TestNewServerKeepsHealthNotServingWhenReadinessFails(t *testing.T) {
	server := NewServer(ServerConfig{Ready: func(context.Context) error { return errors.New("database unavailable") }})
	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial health server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got, err := grpc_health_v1.NewHealthClient(conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("check failed readiness: %v", err)
	}
	if got.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health after failed readiness = %s, want NOT_SERVING", got.Status)
	}
}

type blockingStopper struct {
	gracefulStarted chan struct{}
	release         chan struct{}
	stopped         chan struct{}
}

func (s *blockingStopper) GracefulStop() {
	close(s.gracefulStarted)
	<-s.release
}

func (s *blockingStopper) Stop() {
	close(s.stopped)
	close(s.release)
}

func TestShutdownStopsServerWhenGracefulStopExceedsDeadline(t *testing.T) {
	server := &blockingStopper{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	Shutdown(ctx, server)

	select {
	case <-server.gracefulStarted:
	default:
		t.Fatal("GracefulStop was not started")
	}
	select {
	case <-server.stopped:
	default:
		t.Fatal("Stop was not used when graceful shutdown timed out")
	}
}

func TestNewServerReportsServingOnlyAfterReadinessPasses(t *testing.T) {
	ready := make(chan struct{})
	server := NewServer(ServerConfig{Ready: func(context.Context) error {
		<-ready
		return nil
	}})

	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial health server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := grpc_health_v1.NewHealthClient(conn)

	before, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("check before ready: %v", err)
	}
	if before.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health before readiness = %s, want NOT_SERVING", before.Status)
	}

	close(ready)
	deadline := time.Now().Add(time.Second)
	for {
		after, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("check after ready: %v", err)
		}
		if after.Status == grpc_health_v1.HealthCheckResponse_SERVING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("health did not transition to SERVING after readiness passed")
		}
		time.Sleep(time.Millisecond)
	}
}
