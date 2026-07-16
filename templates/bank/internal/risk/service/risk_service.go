// Package service 是 risk 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/risk/domain"
)

// RiskStore risk 查询接口（repo 实现）。
type RiskStore interface {
	ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error)
	GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error)
	ListRules(ctx context.Context) ([]domain.RiskRule, error)
	ListBlacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error)
}

// RiskService risk 只读服务，包装 RiskStore 做查询编排。
type RiskService struct{ store RiskStore }

// NewRiskService 构造 RiskService。
func NewRiskService(store RiskStore) *RiskService { return &RiskService{store: store} }

// ListEvents 按条件筛选风控事件并分页。
func (s *RiskService) ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error) {
	return s.store.ListEvents(ctx, from, to, ruleID, action, offset, limit)
}

// Event 查风控事件详情（跨库联邦）。
func (s *RiskService) Event(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	return s.store.GetEvent(ctx, eventID)
}

// Rules 列风控规则。
func (s *RiskService) Rules(ctx context.Context) ([]domain.RiskRule, error) { return s.store.ListRules(ctx) }

// Blacklists 按客户筛选黑名单并分页。
func (s *RiskService) Blacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error) {
	return s.store.ListBlacklists(ctx, custID, offset, limit)
}
