// Package service is the use case layer of customer service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/customer/domain"
)

// CustomerStore customer query interface (repo implementation).
type CustomerStore interface {
	GetCustomer(ctx context.Context, custID string) (domain.Customer, error)
	ListCustomers(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error)
	GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error)
}

// CustomerService is a customer read-only service that wraps CustomerStore for query orchestration.
type CustomerService struct{ store CustomerStore }

// NewCustomerService constructs CustomerService.
func NewCustomerService(store CustomerStore) *CustomerService { return &CustomerService{store: store} }

// Get checks a single customer.
func (s *CustomerService) Get(ctx context.Context, custID string) (domain.Customer, error) {
	return s.store.GetCustomer(ctx, custID)
}

// List filtered and paginated by type/kyc.
func (s *CustomerService) List(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error) {
	return s.store.ListCustomers(ctx, custType, kycStatus, offset, limit)
}

// Accounts Check the customer's associated accounts (aggregated through the core-banking service).
func (s *CustomerService) Accounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	return s.store.GetCustAccounts(ctx, custID)
}
