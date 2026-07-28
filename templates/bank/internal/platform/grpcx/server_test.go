package grpcx

import (
	"context"
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
	t.Cleanup(func() { _ = listener.Close() })
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

func TestNewServerUsesStandardHealthCheckAndWatchSemantics(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server := NewServer(ServerConfig{Ready: func(context.Context) error {
		calls.Add(1)
		close(started)
		<-release
		return nil
	}})
	ctx, client := newHealthClient(t, server)
	<-started

	check, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("check health: %v", err)
	}
	if check.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("check status = %s, want NOT_SERVING", check.Status)
	}
	if calls.Load() != 1 {
		t.Fatalf("check invoked readiness %d times, want only lifecycle invocation", calls.Load())
	}

	stream, err := client.Watch(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("watch health: %v", err)
	}
	initial, err := stream.Recv()
	if err != nil {
		t.Fatalf("initial watch update: %v", err)
	}
	if initial.Status != grpc_health_v1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("initial watch status = %s, want NOT_SERVING", initial.Status)
	}
	if calls.Load() != 1 {
		t.Fatalf("watch invoked readiness %d times, want only lifecycle invocation", calls.Load())
	}

	close(release)
	next, err := stream.Recv()
	if err != nil {
		t.Fatalf("serving watch update: %v", err)
	}
	if next.Status != grpc_health_v1.HealthCheckResponse_SERVING {
		t.Fatalf("serving watch status = %s, want SERVING", next.Status)
	}
	server.Stop()
}

func TestShutdownCancelsCooperativeReadiness(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := NewServer(ServerConfig{Ready: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	Shutdown(ctx, server)

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not cancel cooperative readiness")
	}
	waitForReadinessTrackerGone(t, server)
}

func TestReadinessTrackerRetainsNoncooperativeCallbackUntilItExits(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := NewServer(ServerConfig{Ready: func(context.Context) error {
		close(started)
		<-release
		return nil
	}})
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	Shutdown(ctx, server)
	if !hasReadinessTracker(server) {
		t.Fatal("tracker was removed while noncooperative readiness callback still ran")
	}
	close(release)
	waitForReadinessTrackerGone(t, server)
}

func waitForReadinessTrackerGone(t *testing.T, server *grpc.Server) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for hasReadinessTracker(server) {
		if time.Now().After(deadline) {
			t.Fatal("readiness tracker did not clear after callback exit")
		}
		time.Sleep(time.Millisecond)
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
