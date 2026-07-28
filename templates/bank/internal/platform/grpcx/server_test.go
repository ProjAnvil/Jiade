package grpcx

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func newHealthClient(t *testing.T, server *grpc.Server) (context.Context, grpc_health_v1.HealthClient) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() {
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
	return ctx, grpc_health_v1.NewHealthClient(conn)
}

func TestNewServerRetriesReadinessUntilHealthIsServing(t *testing.T) {
	originalInterval := readinessProbeInterval
	readinessProbeInterval = time.Millisecond
	t.Cleanup(func() { readinessProbeInterval = originalInterval })

	var calls atomic.Int32
	server := NewServer(ServerConfig{Ready: func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("database unavailable")
		}
		return nil
	}})
	t.Cleanup(server.Stop)
	ctx, client := newHealthClient(t, server)

	stream, err := client.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("watch health: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first health update: %v", err)
	}
	if first.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("first health update = %s, want NOT_SERVING", first.Status)
	}
	second, err := stream.Recv()
	if err != nil {
		t.Fatalf("second health update: %v", err)
	}
	if second.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("second health update = %s, want SERVING", second.Status)
	}
}

func TestNewServerCancelsTimedOutReadinessDuringCheck(t *testing.T) {
	originalTimeout := readinessTimeout
	readinessTimeout = 10 * time.Millisecond
	t.Cleanup(func() { readinessTimeout = originalTimeout })

	canceled := make(chan struct{})
	server := NewServer(ServerConfig{Ready: func(ctx context.Context) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}})
	t.Cleanup(server.Stop)
	ctx, client := newHealthClient(t, server)

	got, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if got.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health after timed-out readiness = %s, want NOT_SERVING", got.Status)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("readiness callback was not canceled after its timeout")
	}
}

type blockingStopper struct {
	gracefulStarted chan struct{}
	stopped         chan struct{}
}

func (s *blockingStopper) GracefulStop() {
	close(s.gracefulStarted)
	select {}
}

func (s *blockingStopper) Stop() { close(s.stopped) }

func TestShutdownReturnsWhenStopCannotUnblockGracefulStop(t *testing.T) {
	server := &blockingStopper{make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		Shutdown(ctx, server)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Shutdown waited for a GracefulStop that Stop cannot unblock")
	}
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

func TestShutdownReturnsWhenHealthCallbackIgnoresCancellation(t *testing.T) {
	started := make(chan struct{})
	server := NewServer(ServerConfig{Ready: func(context.Context) error {
		close(started)
		select {}
	}})
	ctx, client := newHealthClient(t, server)
	checkDone := make(chan struct{})
	go func() {
		_, _ = client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		close(checkDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("readiness callback did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		Shutdown(shutdownCtx, server)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after Stop")
	}
	select {
	case <-checkDone:
	case <-time.After(time.Second):
		t.Fatal("forced Stop did not terminate the health RPC")
	}
}
