// Package service is the use case layer of risk service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/risk/domain"
)

// RiskStore risk query interface (repo implementation).
type RiskStore interface {
	ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error)
	GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error)
	ListRules(ctx context.Context) ([]domain.RiskRule, error)
	ListBlacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error)
}

// RiskService risk is a read-only service that wraps RiskStore for query orchestration.
type RiskService struct{ store RiskStore }

// NewRiskService constructs RiskService.
func NewRiskService(store RiskStore) *RiskService { return &RiskService{store: store} }

// ListEvents filters risk control events according to conditions and paging them.
func (s *RiskService) ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error) {
	return s.store.ListEvents(ctx, from, to, ruleID, action, offset, limit)
}

// Event Check risk control event details (aggregated through customer service).
func (s *RiskService) Event(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	return s.store.GetEvent(ctx, eventID)
}

// Rules lists risk control rules.
func (s *RiskService) Rules(ctx context.Context) ([]domain.RiskRule, error) {
	return s.store.ListRules(ctx)
}

// Blacklists Filter and paginate blacklists by customer.
func (s *RiskService) Blacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error) {
	return s.store.ListBlacklists(ctx, custID, offset, limit)
}
