// Package service 是 loan 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/loan/domain"
)

// LoanStore loan 查询接口（repo 实现）。
type LoanStore interface {
	ListProducts(ctx context.Context) ([]domain.LoanProduct, error)
	ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error)
	GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error)
	ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error)
	ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error)
	GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error)
}

// LoanService loan 只读服务，包装 LoanStore 做查询编排。
type LoanService struct{ store LoanStore }

// NewLoanService 构造 LoanService。
func NewLoanService(store LoanStore) *LoanService { return &LoanService{store: store} }

// ListProducts 列贷款产品。
func (s *LoanService) ListProducts(ctx context.Context) ([]domain.LoanProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListAccounts 按产品/状态筛选借据并分页。
func (s *LoanService) ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	return s.store.ListAccounts(ctx, productCode, status, offset, limit)
}

// GetAccount 查单个借据。
func (s *LoanService) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	return s.store.GetAccount(ctx, loanNo)
}

// ListBalances 按日期范围查逐日余额快照。
func (s *LoanService) ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error) {
	return s.store.ListBalances(ctx, from, to, loanNo, offset, limit)
}

// ListOverdue 按五级分类/日期范围查逾期。
func (s *LoanService) ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error) {
	return s.store.ListOverdue(ctx, overdueClass, from, to, offset, limit)
}

// Profile 查借据档案（跨库联邦）。
func (s *LoanService) Profile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	return s.store.GetProfile(ctx, loanNo)
}
