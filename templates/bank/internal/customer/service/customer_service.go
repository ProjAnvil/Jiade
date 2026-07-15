// Package service 是 customer 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/customer/domain"
)

// CustomerStore 客户查询接口（repo 实现）。
type CustomerStore interface {
	GetCustomer(ctx context.Context, custID string) (domain.Customer, error)
	ListCustomers(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error)
	GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error)
}

// CustomerService 客户只读服务，包装 CustomerStore 做查询编排。
type CustomerService struct{ store CustomerStore }

// NewCustomerService 构造 CustomerService。
func NewCustomerService(store CustomerStore) *CustomerService { return &CustomerService{store: store} }

// Get 查单个客户。
func (s *CustomerService) Get(ctx context.Context, custID string) (domain.Customer, error) {
	return s.store.GetCustomer(ctx, custID)
}

// List 按类型/kyc 筛选并分页。
func (s *CustomerService) List(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error) {
	return s.store.ListCustomers(ctx, custType, kycStatus, offset, limit)
}

// Accounts 查客户的关联账户（跨库联邦）。
func (s *CustomerService) Accounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	return s.store.GetCustAccounts(ctx, custID)
}
