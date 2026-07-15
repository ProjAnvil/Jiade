// Package service 是 payment 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/payment/domain"
)

// PaymentStore payment 查询接口（repo 实现）。单测用 fakePayRepo 注入。
type PaymentStore interface {
	ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error)
	GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error)
	GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error)
	GetMerchant(ctx context.Context, merchantID string) (domain.Merchant, error)
}

// PaymentService payment 只读服务，包装 PaymentStore 做查询编排。
type PaymentService struct{ store PaymentStore }

// NewPaymentService 构造 PaymentService。
func NewPaymentService(store PaymentStore) *PaymentService { return &PaymentService{store: store} }

// ListTransfers 按账户/日期筛选转账并分页。
func (s *PaymentService) ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error) {
	return s.store.ListTransfers(ctx, accountNo, from, to, limit, offset)
}

// GetTransfer 查单笔转账。
func (s *PaymentService) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	return s.store.GetTransfer(ctx, txnID)
}

// GetParties 查转账双方（跨库联邦）。
func (s *PaymentService) GetParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	return s.store.GetTransferParties(ctx, txnID)
}

// GetMerchant 查商户。
func (s *PaymentService) GetMerchant(ctx context.Context, id string) (domain.Merchant, error) {
	return s.store.GetMerchant(ctx, id)
}
