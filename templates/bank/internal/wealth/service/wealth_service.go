// Package service is the use case layer of wealth service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/wealth/domain"
)

// WealthStore wealth query interface (repo implementation).
type WealthStore interface {
	ListProducts(ctx context.Context) ([]domain.WealthProduct, error)
	ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error)
	ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error)
	ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error)
	ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error)
	GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error)
}

// WealthService wealth is a read-only service that wraps WealthStore for query orchestration.
type WealthService struct{ store WealthStore }

// NewWealthService constructs WealthService.
func NewWealthService(store WealthStore) *WealthService { return &WealthService{store: store} }

// ListProducts lists financial products.
func (s *WealthService) ListProducts(ctx context.Context) ([]domain.WealthProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListNav Check daily net value by product/date range.
func (s *WealthService) ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error) {
	return s.store.ListNav(ctx, productCode, from, to)
}

// ListHoldings Filter and paginate holdings by client.
func (s *WealthService) ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	return s.store.ListHoldings(ctx, custID, offset, limit)
}

// ListOrders Check orders by customer/product/date range.
func (s *WealthService) ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error) {
	return s.store.ListOrders(ctx, custID, productCode, from, to, offset, limit)
}

// ListIncomes checks income by position/date range.
func (s *WealthService) ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error) {
	return s.store.ListIncomes(ctx, holdingID, from, to, offset, limit)
}

// HoldingProfile checks the holding profile (aggregated through the customer service).
func (s *WealthService) HoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	return s.store.GetHoldingProfile(ctx, holdingID)
}
