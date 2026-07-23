// Package service is the use case layer of payment service (query orchestration, pure logic can be tested individually).
package service

import (
	"context"

	"bank/internal/payment/domain"
)

// PaymentStore payment query interface (repo implementation). Use fakePayRepo to inject single test.
type PaymentStore interface {
	ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error)
	GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error)
	GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error)
	GetMerchant(ctx context.Context, merchantID string) (domain.Merchant, error)
}

// PaymentService payment is a read-only service that wraps PaymentStore for query arrangement.
type PaymentService struct{ store PaymentStore }

// NewPaymentService constructs PaymentService.
func NewPaymentService(store PaymentStore) *PaymentService { return &PaymentService{store: store} }

// ListTransfers Filter and paginate transfers by account/date.
func (s *PaymentService) ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error) {
	return s.store.ListTransfers(ctx, accountNo, from, to, limit, offset)
}

// GetTransfer checks a single transfer.
func (s *PaymentService) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	return s.store.GetTransfer(ctx, txnID)
}

// GetParties checks the transfer parties (aggregated through the core-banking/customer service).
func (s *PaymentService) GetParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	return s.store.GetTransferParties(ctx, txnID)
}

// GetMerchant Check merchants.
func (s *PaymentService) GetMerchant(ctx context.Context, id string) (domain.Merchant, error) {
	return s.store.GetMerchant(ctx, id)
}
