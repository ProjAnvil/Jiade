package repo

import (
	"context"
	"net"
	"testing"
	"time"

	corev1 "bank/gen/bank/core/v1"
	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/platform/serviceclient"

	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type requestIDAccountServer struct {
	corev1.UnimplementedAccountQueryServiceServer
	t *testing.T
}

func (s requestIDAccountServer) GetAccount(_ context.Context, req *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
	if req.GetRequestId() != "payment-request-7" {
		s.t.Fatalf("account request_id = %q, want payment-request-7", req.GetRequestId())
	}
	return &corev1.AccountSnapshot{AccountNo: req.GetAccountNo(), CustomerId: "C-7"}, nil
}

type requestIDCustomerServer struct {
	customerv1.UnimplementedCustomerQueryServiceServer
	t *testing.T
}

func (s requestIDCustomerServer) GetCustomer(_ context.Context, req *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
	if req.GetRequestId() != "payment-request-7" {
		s.t.Fatalf("customer request_id = %q, want payment-request-7", req.GetRequestId())
	}
	return &customerv1.CustomerSnapshot{CustomerId: req.GetCustomerId(), Name: "Ada"}, nil
}

func TestCustomerNameForAccountForwardsChiRequestIDToGRPC(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	corev1.RegisterAccountQueryServiceServer(server, requestIDAccountServer{t: t})
	customerv1.RegisterCustomerQueryServiceServer(server, requestIDCustomerServer{t: t})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	r := &PaymentRepo{
		core:     serviceclient.NewAccountReader(corev1.NewAccountQueryServiceClient(conn)),
		customer: serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(conn)),
	}
	ctx = context.WithValue(ctx, middleware.RequestIDKey, "payment-request-7")
	name, err := r.customerNameForAccount(ctx, "D-7")
	if err != nil {
		t.Fatalf("customerNameForAccount: %v", err)
	}
	if name != "Ada" {
		t.Fatalf("name = %q, want Ada", name)
	}
}
