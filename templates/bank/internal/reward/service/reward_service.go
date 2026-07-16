// Package service 是 reward 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/reward/domain"
)

// RewardStore reward 查询接口（repo 实现）。
type RewardStore interface {
	GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error)
	ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error)
	ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error)
	GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error)
}

// RewardService reward 只读服务，包装 RewardStore 做查询编排。
type RewardService struct{ store RewardStore }

// NewRewardService 构造 RewardService。
func NewRewardService(store RewardStore) *RewardService { return &RewardService{store: store} }

// GetPointsAcct 查单个积分账户。
func (s *RewardService) GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error) {
	return s.store.GetPointsAcct(ctx, custID)
}

// ListPointsAccts 按会员等级筛选并分页。
func (s *RewardService) ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error) {
	return s.store.ListPointsAccts(ctx, memberLevel, offset, limit)
}

// ListCoupons 查客户优惠券。
func (s *RewardService) ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error) {
	return s.store.ListCoupons(ctx, custID, status, offset, limit)
}

// Profile 查积分档案（跨库联邦）。
func (s *RewardService) Profile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	return s.store.GetProfile(ctx, custID)
}
