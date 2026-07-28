package api

import (
	"context"
	"database/sql"
	"testing"

	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/customer/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type customerQueryStore struct {
	customer domain.Customer
	err      error
}

func (s customerQueryStore) GetCustomer(context.Context, string) (domain.Customer, error) {
	return s.customer, s.err
}

func TestCustomerQueryServerMapsCustomerSnapshot(t *testing.T) {
	server := NewCustomerQueryServer(customerQueryStore{customer: domain.Customer{
		CustID: "C-42", Name: "Ada", CustType: domain.CustTypePersonal,
		KYCStatus: "passed", Status: "restricted", RiskTags: []string{"pep", "sanctions"},
	}})

	got, err := server.GetCustomer(context.Background(), &customerv1.GetCustomerRequest{CustomerId: "C-42"})
	if err != nil {
		t.Fatalf("GetCustomer: %v", err)
	}
	if got.CustomerId != "C-42" || got.Name != "Ada" || got.CustomerType != "个人" || got.KycStatus != "passed" || got.Status != "restricted" {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(got.RiskTags) != 2 || got.RiskTags[0] != "pep" || got.RiskTags[1] != "sanctions" {
		t.Fatalf("risk tags = %#v, want [pep sanctions]", got.RiskTags)
	}
}

func TestCustomerQueryServerMapsMissingCustomerToNotFound(t *testing.T) {
	server := NewCustomerQueryServer(customerQueryStore{err: sql.ErrNoRows})

	_, err := server.GetCustomer(context.Background(), &customerv1.GetCustomerRequest{CustomerId: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.NotFound, err)
	}
}
