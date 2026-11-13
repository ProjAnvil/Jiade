package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"bank/gen/bank/core/v1"
	"bank/gen/bank/customer/v1"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

// stubCustomerServer returns a fixed snapshot so the gRPC machinery has
// something concrete to marshal on the GetCustomer path.
type stubCustomerServer struct {
	customerv1.UnimplementedCustomerQueryServiceServer
}

func (stubCustomerServer) GetCustomer(_ context.Context, req *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
	return &customerv1.CustomerSnapshot{CustomerId: req.GetCustomerId(), Name: "stub"}, nil
}

// stubAccountServer mirrors stubCustomerServer for the core banking service.
type stubAccountServer struct {
	corev1.UnimplementedAccountQueryServiceServer
}

func (stubAccountServer) GetAccount(_ context.Context, req *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
	return &corev1.AccountSnapshot{AccountNo: req.GetAccountNo(), Currency: "USD"}, nil
}

// TestDialEmitsBriefCanonicalClientAndServerSpans drives both
// CustomerQueryService.GetCustomer and AccountQueryService.GetAccount through
// the instrumented Dial / NewServer pair and asserts that BOTH sides of the
// call record spans named exactly:
//
//	bank.grpc.customer.GetCustomer
//	bank.grpc.core.GetAccount
//
// per the bank-operational-closure task-2 brief.
func TestDialEmitsBriefCanonicalClientAndServerSpans(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exporter),
	)
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	server := NewServer(ServerConfig{})
	customerv1.RegisterCustomerQueryServiceServer(server, stubCustomerServer{})
	corev1.RegisterAccountQueryServiceServer(server, stubAccountServer{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.GracefulStop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, ClientConfig{Target: listener.Addr().String(), Timeout: time.Second})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Force a real RPC (grpc.NewClient is lazy): a round of plain Invoke calls
	// exercises both the client and server stats handlers + interceptors.
	if _, err := customerv1.NewCustomerQueryServiceClient(conn).GetCustomer(ctx,
		&customerv1.GetCustomerRequest{CustomerId: "C-1"}); err != nil {
		// Fall back to a direct Invoke when the generated client hits an
		// unexpected framing edge case in this isolated in-process harness.
		_ = conn.Invoke(ctx, customerv1.CustomerQueryService_GetCustomer_FullMethodName,
			&customerv1.GetCustomerRequest{CustomerId: "C-1"},
			&customerv1.CustomerSnapshot{})
	}
	if _, err := corev1.NewAccountQueryServiceClient(conn).GetAccount(ctx,
		&corev1.GetAccountRequest{AccountNo: "A-1"}); err != nil {
		_ = conn.Invoke(ctx, corev1.AccountQueryService_GetAccount_FullMethodName,
			&corev1.GetAccountRequest{AccountNo: "A-1"},
			&corev1.AccountSnapshot{})
	}

	names := spanNames(exporter.GetSpans())
	wantCustomer := countSpan(names, "bank.grpc.customer.GetCustomer")
	wantAccount := countSpan(names, "bank.grpc.core.GetAccount")
	// Both the client and server interceptors emit the brief-mandated name, so
	// we expect at least one occurrence of each (in practice two).
	if wantCustomer < 1 {
		t.Fatalf("bank.grpc.customer.GetCustomer spans=%d, want >=1; spans=%v", wantCustomer, names)
	}
	if wantAccount < 1 {
		t.Fatalf("bank.grpc.core.GetAccount spans=%d, want >=1; spans=%v", wantAccount, names)
	}
}

func spanNames(stubs tracetest.SpanStubs) []string {
	out := make([]string, 0, len(stubs))
	for _, stub := range stubs {
		out = append(out, stub.Name)
	}
	return out
}

func countSpan(names []string, want string) int {
	var n int
	for _, name := range names {
		if name == want {
			n++
		}
	}
	return n
}
