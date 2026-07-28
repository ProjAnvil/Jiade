package api

import (
	"context"
	"database/sql"
	"errors"

	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/customer/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CustomerQueryReader is the customer read dependency used by the internal
// gRPC query service.
type CustomerQueryReader interface {
	GetCustomer(context.Context, string) (domain.Customer, error)
}

// CustomerQueryServer exposes customer snapshots to internal callers.
type CustomerQueryServer struct {
	customerv1.UnimplementedCustomerQueryServiceServer
	customers CustomerQueryReader
}

// NewCustomerQueryServer creates the internal customer query adapter.
func NewCustomerQueryServer(customers CustomerQueryReader) *CustomerQueryServer {
	return &CustomerQueryServer{customers: customers}
}

// GetCustomer maps the repository customer to the internal protobuf snapshot.
func (s *CustomerQueryServer) GetCustomer(ctx context.Context, req *customerv1.GetCustomerRequest) (*customerv1.CustomerSnapshot, error) {
	customer, err := s.customers.GetCustomer(ctx, req.GetCustomerId())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "customer not found")
		}
		return nil, status.Error(codes.Internal, "get customer")
	}

	riskTags := []string(nil)
	if customer.RiskLevel != "" {
		riskTags = []string{customer.RiskLevel}
	}
	return &customerv1.CustomerSnapshot{
		CustomerId:   customer.CustID,
		Name:         customer.Name,
		CustomerType: string(customer.CustType),
		KycStatus:    customer.KYCStatus,
		Status:       "active",
		RiskTags:     riskTags,
	}, nil
}
