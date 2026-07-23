// Package service is the use case layer of the loan service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/loan/domain"
)

// LoanStore loan query interface (repo implementation).
type LoanStore interface {
	ListProducts(ctx context.Context) ([]domain.LoanProduct, error)
	ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error)
	GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error)
	ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error)
	ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error)
	GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error)
}

// LoanService loan is a read-only service that wraps LoanStore for query orchestration.
type LoanService struct{ store LoanStore }

// NewLoanService constructs LoanService.
func NewLoanService(store LoanStore) *LoanService { return &LoanService{store: store} }

// ListProducts lists loan products.
func (s *LoanService) ListProducts(ctx context.Context) ([]domain.LoanProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListAccounts Filter and paginate IOUs by product/status.
func (s *LoanService) ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	return s.store.ListAccounts(ctx, productCode, status, offset, limit)
}

// GetAccount checks a single IOU.
func (s *LoanService) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	return s.store.GetAccount(ctx, loanNo)
}

// ListBalances Check daily balance snapshots by date range.
func (s *LoanService) ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error) {
	return s.store.ListBalances(ctx, from, to, loanNo, offset, limit)
}

// ListOverdue checks overdue items by five-level classification/date range.
func (s *LoanService) ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error) {
	return s.store.ListOverdue(ctx, overdueClass, from, to, offset, limit)
}

// Profile checks the IOU file (aggregated through the customer service).
func (s *LoanService) Profile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	return s.store.GetProfile(ctx, loanNo)
}
