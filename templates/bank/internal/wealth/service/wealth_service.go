// Package service 是 wealth 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/wealth/domain"
)

// WealthStore wealth 查询接口（repo 实现）。
type WealthStore interface {
	ListProducts(ctx context.Context) ([]domain.WealthProduct, error)
	ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error)
	ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error)
	ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error)
	ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error)
	GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error)
}

// WealthService wealth 只读服务，包装 WealthStore 做查询编排。
type WealthService struct{ store WealthStore }

// NewWealthService 构造 WealthService。
func NewWealthService(store WealthStore) *WealthService { return &WealthService{store: store} }

// ListProducts 列理财产品。
func (s *WealthService) ListProducts(ctx context.Context) ([]domain.WealthProduct, error) {
	return s.store.ListProducts(ctx)
}

// ListNav 按产品/日期范围查每日净值。
func (s *WealthService) ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error) {
	return s.store.ListNav(ctx, productCode, from, to)
}

// ListHoldings 按客户筛选持仓并分页。
func (s *WealthService) ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	return s.store.ListHoldings(ctx, custID, offset, limit)
}

// ListOrders 按客户/产品/日期范围查订单。
func (s *WealthService) ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error) {
	return s.store.ListOrders(ctx, custID, productCode, from, to, offset, limit)
}

// ListIncomes 按持仓/日期范围查收益。
func (s *WealthService) ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error) {
	return s.store.ListIncomes(ctx, holdingID, from, to, offset, limit)
}

// HoldingProfile 查持仓档案（跨库联邦）。
func (s *WealthService) HoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	return s.store.GetHoldingProfile(ctx, holdingID)
}
