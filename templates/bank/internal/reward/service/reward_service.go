// Package service is the use case layer of reward service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/reward/domain"
)

// RewardStore reward query interface (repo implementation).
type RewardStore interface {
	GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error)
	ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error)
	ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error)
	GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error)
}

// RewardService reward is a read-only service that wraps RewardStore for query orchestration.
type RewardService struct{ store RewardStore }

// NewRewardService constructs RewardService.
func NewRewardService(store RewardStore) *RewardService { return &RewardService{store: store} }

// GetPointsAcct checks a single points account.
func (s *RewardService) GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error) {
	return s.store.GetPointsAcct(ctx, custID)
}

// ListPointsAccts Filter and paginate by membership level.
func (s *RewardService) ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error) {
	return s.store.ListPointsAccts(ctx, memberLevel, offset, limit)
}

// ListCoupons Check customer coupons.
func (s *RewardService) ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error) {
	return s.store.ListCoupons(ctx, custID, status, offset, limit)
}

// Profile checks the points file (aggregated through the customer service).
func (s *RewardService) Profile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	return s.store.GetProfile(ctx, custID)
}
