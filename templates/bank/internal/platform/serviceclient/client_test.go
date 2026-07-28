package serviceclient

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	corev1 "bank/gen/bank/core/v1"
	customerv1 "bank/gen/bank/customer/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

func dialBufconn(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	register(server)
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
	return conn
}

type customerQueryServer struct {
	customerv1.UnimplementedCustomerQueryServiceServer
	get func(context.Context, *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error)
}

func (s customerQueryServer) GetCustomer(ctx context.Context, req *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
	return s.get(ctx, req)
}

type accountQueryServer struct {
	corev1.UnimplementedAccountQueryServiceServer
	get func(context.Context, *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error)
}

func (s accountQueryServer) GetAccount(ctx context.Context, req *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
	return s.get(ctx, req)
}

func TestCustomerReaderMapsSnapshotAndRequestID(t *testing.T) {
	conn := dialBufconn(t, func(server *grpc.Server) {
		customerv1.RegisterCustomerQueryServiceServer(server, customerQueryServer{get: func(_ context.Context, req *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
			if req.GetCustomerId() != "C-7" || req.GetRequestId() != "request-9" {
				t.Fatalf("request = %#v", req)
			}
			return &customerv1.CustomerSnapshot{CustomerId: "C-7", Name: "Ada", CustomerType: "individual", KycStatus: "verified", Status: "active", RiskTags: []string{"pep", "high-risk"}}, nil
		}})
	})

	got, err := NewCustomerReader(customerv1.NewCustomerQueryServiceClient(conn)).GetCustomer(context.Background(), "C-7", "request-9")
	if err != nil {
		t.Fatalf("GetCustomer: %v", err)
	}
	if want := (Customer{CustomerID: "C-7", Name: "Ada", CustomerType: "individual", KYCStatus: "verified", Status: "active", RiskTags: []string{"pep", "high-risk"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("customer = %#v", got)
	}
}

func TestCustomerReaderMapsNotFoundToDomainNotFound(t *testing.T) {
	conn := dialBufconn(t, func(server *grpc.Server) {
		customerv1.RegisterCustomerQueryServiceServer(server, customerQueryServer{get: func(context.Context, *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
			return nil, status.Error(codes.NotFound, "missing")
		}})
	})

	_, err := NewCustomerReader(customerv1.NewCustomerQueryServiceClient(conn)).GetCustomer(context.Background(), "missing", "")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("error = %v, want sql.ErrNoRows", err)
	}
}

func TestAccountReaderMapsSnapshot(t *testing.T) {
	conn := dialBufconn(t, func(server *grpc.Server) {
		corev1.RegisterAccountQueryServiceServer(server, accountQueryServer{get: func(_ context.Context, req *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
			if req.GetAccountNo() != "A-4" || req.GetRequestId() != "request-5" {
				t.Fatalf("request = %#v", req)
			}
			return &corev1.AccountSnapshot{AccountNo: "A-4", CustomerId: "C-7", Currency: "CNY", Status: "active", LedgerBalanceMinor: 1250, AvailableBalanceMinor: 1100}, nil
		}})
	})

	got, err := NewAccountReader(corev1.NewAccountQueryServiceClient(conn)).GetAccount(context.Background(), "A-4", "request-5")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got != (Account{AccountNo: "A-4", CustomerID: "C-7", Currency: "CNY", Status: "active", LedgerBalanceMinor: 1250, AvailableBalanceMinor: 1100}) {
		t.Fatalf("account = %#v", got)
	}
}

func TestAccountReaderMapsNotFoundAndLeavesUnavailableRetryable(t *testing.T) {
	for name, rpcErr := range map[string]error{
		"not found":   status.Error(codes.NotFound, "missing"),
		"unavailable": status.Error(codes.Unavailable, "retry later"),
	} {
		t.Run(name, func(t *testing.T) {
			conn := dialBufconn(t, func(server *grpc.Server) {
				corev1.RegisterAccountQueryServiceServer(server, accountQueryServer{get: func(context.Context, *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
					return nil, rpcErr
				}})
			})
			_, err := NewAccountReader(corev1.NewAccountQueryServiceClient(conn)).GetAccount(context.Background(), "missing", "")
			if name == "not found" && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("error = %v, want sql.ErrNoRows", err)
			}
			if name == "unavailable" && status.Code(err) != codes.Unavailable {
				t.Fatalf("error code = %s, want Unavailable (err=%v)", status.Code(err), err)
			}
		})
	}
}
